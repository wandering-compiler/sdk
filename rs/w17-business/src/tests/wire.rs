//! The runtime over the wire: a tier booted from a manifest, serving the
//! SDK's own `W17Files` service as its "business" service (the descriptor
//! set is the SDK's), in front of a fake transaction coordinator.

use std::{
    collections::HashMap,
    sync::{
        Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use futures::StreamExt as _;
use tokio::sync::oneshot;
use tonic::{Code, Request, Response, Status};

use crate::{
    BootError, Clients, Config, Ctx, Registry, SelfChannel, distx, for_user_dev, on_commit,
    pb::w17::common::{
        distx::{
            BeginRequest, BeginResponse, CommitRequest, CommitResponse, RollbackRequest,
            RollbackResponse,
            w17_distributed_transaction_server::{
                W17DistributedTransaction, W17DistributedTransactionServer,
            },
        },
        files::{
            DeleteRequest, DeleteResponse, OpenRequest, OpenResponse, PutRequest, PutResponse,
            StatRequest, StatResponse,
            w17_files_client::W17FilesClient,
            w17_files_server::{W17Files, W17FilesServer},
        },
    },
    runtime::{probe, serve_for_test},
    secrets::Env,
};

// --- the fake coordinator ---------------------------------------------------------

static CALLS: Mutex<Vec<String>> = Mutex::new(Vec::new());
static COMMIT_UNAVAILABLE: AtomicBool = AtomicBool::new(false);
/// The coordinator's error statuses leave after response headers (in
/// trailers), as a w17 Go server answers a status its handler returned.
static STATUS_AFTER_HEADERS: AtomicBool = AtomicBool::new(false);
static HOOK_RAN: AtomicUsize = AtomicUsize::new(0);

fn calls() -> Vec<String> {
    CALLS.lock().unwrap().clone()
}

fn record(s: String) {
    CALLS.lock().unwrap().push(s);
}

struct Coordinator;

#[tonic::async_trait]
impl W17DistributedTransaction for Coordinator {
    async fn begin(&self, req: Request<BeginRequest>) -> Result<Response<BeginResponse>, Status> {
        let conn = req.into_inner().connection_name;
        record(format!("begin {conn}"));
        let mut resp = Response::new(BeginResponse {
            conn_id: String::new(),
            tx_id: format!("tx-{conn}"),
        });
        resp.metadata_mut()
            .insert("w17-conn-id", "c-1".parse().unwrap());
        Ok(resp)
    }

    async fn commit(
        &self,
        req: Request<CommitRequest>,
    ) -> Result<Response<CommitResponse>, Status> {
        let tx = req
            .metadata()
            .get("w17-tx-id")
            .map(|v| v.to_str().unwrap().to_string())
            .unwrap_or_default();
        record(format!("commit {} {tx}", req.into_inner().tx_id));
        if COMMIT_UNAVAILABLE.load(Ordering::SeqCst) {
            return Err(Status::unavailable("reply lost"));
        }
        Ok(Response::new(CommitResponse {}))
    }

    async fn rollback(
        &self,
        req: Request<RollbackRequest>,
    ) -> Result<Response<RollbackResponse>, Status> {
        record(format!("rollback {}", req.into_inner().tx_id));
        if COMMIT_UNAVAILABLE.load(Ordering::SeqCst) {
            return Err(Status::not_found("unknown tx_id"));
        }
        Ok(Response::new(RollbackResponse {}))
    }
}

// --- the tier under test ----------------------------------------------------------

struct Biz {
    tx: distx::Coordinator,
    me: W17FilesClient<SelfChannel>,
    coord: crate::pb::w17::common::distx::w17_distributed_transaction_client::W17DistributedTransactionClient<
        crate::StorageChannel,
    >,
}

fn register(_cfg: &Config, registry: &mut Registry, clients: &Clients) -> Result<(), BootError> {
    registry.register(W17FilesServer::new(Biz {
        tx: clients.distributed_transaction()?,
        me: W17FilesClient::new(registry.self_channel()),
        coord: crate::pb::w17::common::distx::w17_distributed_transaction_client::W17DistributedTransactionClient::new(
            clients.storage(),
        ),
    }))
}

fn register_twice(
    cfg: &Config,
    registry: &mut Registry,
    clients: &Clients,
) -> Result<(), BootError> {
    register(cfg, registry, clients)?;
    register(cfg, registry, clients)
}

#[tonic::async_trait]
impl W17Files for Biz {
    // A client stream through the middleware: the context is there, the
    // chunks arrive in order, and the answer counts them.
    async fn put(
        &self,
        req: Request<tonic::Streaming<PutRequest>>,
    ) -> Result<Response<PutResponse>, Status> {
        let ctx = crate::ctx(&req)?;
        let mut s = req.into_inner();
        let mut size = 0;
        while let Some(part) = s.message().await? {
            if let Some(crate::pb::w17::common::files::put_request::Part::Chunk(c)) = part.part {
                size += c.len();
            }
        }
        Ok(Response::new(PutResponse {
            handle: format!("{size} {}", ctx.incoming().has("x-custom")),
        }))
    }

    type OpenStream = futures::stream::BoxStream<'static, Result<OpenResponse, Status>>;

    async fn open(&self, req: Request<OpenRequest>) -> Result<Response<Self::OpenStream>, Status> {
        // A stream that fails after its first message: a panic, or a
        // for_user status, raised while the body is already being written.
        let fail = req.into_inner().handle;
        if !fail.is_empty() {
            let s = futures::stream::unfold(0, move |i| {
                let fail = fail.clone();
                async move {
                    if i == 1 {
                        // Let the first message leave on its own frame.
                        tokio::time::sleep(Duration::from_millis(50)).await;
                    }
                    match i {
                        0 => Some((Ok(OpenResponse { chunk: vec![1] }), 1)),
                        1 if fail == "panic" => panic!("a stream panicked"),
                        1 => Some((
                            Err(for_user_dev(
                                Code::FailedPrecondition,
                                "GONE",
                                "Gone.",
                                "",
                                "dev text",
                            )),
                            2,
                        )),
                        _ => None,
                    }
                }
            });
            return Ok(Response::new(s.boxed()));
        }
        let s = futures::stream::unfold(0, |i| async move {
            tokio::time::sleep(Duration::from_millis(100)).await;
            (i < 50).then(|| (Ok(OpenResponse { chunk: vec![1] }), i + 1))
        });
        Ok(Response::new(s.boxed()))
    }

    async fn stat(&self, req: Request<StatRequest>) -> Result<Response<StatResponse>, Status> {
        let ctx = crate::ctx(&req)?;
        let handle = req.into_inner().handle;
        let size = match handle.as_str() {
            "panic" => panic!("a handler panicked"),
            "user" => {
                return Err(for_user_dev(
                    Code::FailedPrecondition,
                    "LOCKED",
                    "Locked.",
                    "f",
                    "dev text",
                ));
            }
            "sleep" => {
                tokio::time::sleep(Duration::from_secs(5)).await;
                0
            }
            // The handler's own UNAVAILABLE, after work it may have done.
            "unavailable" => return Err(Status::unavailable("the upstream went away")),
            "ctx" => {
                i64::from(ctx.deadline().is_some()) + 10 * i64::from(ctx.incoming().has("x-custom"))
            }
            "self" => {
                let r = StatRequest {
                    connection: String::new(),
                    handle: "ctx".into(),
                };
                self.me
                    .clone()
                    .stat(ctx.request(r))
                    .await?
                    .into_inner()
                    .size
            }
            "self-no-ctx" => {
                let r = StatRequest {
                    connection: String::new(),
                    handle: "ctx".into(),
                };
                self.me
                    .clone()
                    .stat(Request::new(r))
                    .await?
                    .into_inner()
                    .size
            }
            "no-ctx" => {
                self.coord
                    .clone()
                    .commit(Request::new(CommitRequest { tx_id: "x".into() }))
                    .await?;
                0
            }
            _ => -1,
        };
        Ok(Response::new(StatResponse {
            size,
            mod_time: None,
        }))
    }

    async fn delete(
        &self,
        req: Request<DeleteRequest>,
    ) -> Result<Response<DeleteResponse>, Status> {
        let ctx = crate::ctx(&req)?;
        let handle = req.into_inner().handle;
        distx::run(
            &ctx,
            &self.tx,
            distx::begin_request("main"),
            async |tx_ctx: Ctx| {
                // A nested run joins: no second begin.
                distx::run(
                    &tx_ctx,
                    &self.tx,
                    distx::begin_request("main"),
                    async |_c: Ctx| Ok::<_, Status>(()),
                )
                .await?;
                if handle == "panic-hook" {
                    // An author's hook that panics, parked BEFORE the counting
                    // one (and the method's own events).
                    let loud = handle.clone();
                    on_commit(&tx_ctx, async move {
                        if !loud.is_empty() {
                            panic!("author hook panicked: secret detail");
                        }
                    });
                }
                on_commit(&tx_ctx, async {
                    HOOK_RAN.fetch_add(1, Ordering::SeqCst);
                });
                match handle.as_str() {
                    // The handler never returns: only its caller's deadline
                    // ends the call, by dropping this future.
                    "hang" => futures::future::pending().await,
                    "fail" => Err(Status::aborted("refused after the writes")),
                    "mismatch" => distx::begin(&tx_ctx, &self.tx, distx::begin_request("other"))
                        .await
                        .map(|_| ())
                        .map_err(Into::into),
                    _ => Ok(()),
                }
            },
        )
        .await?;
        Ok(Response::new(DeleteResponse {}))
    }
}

// --- harness ----------------------------------------------------------------------

fn free_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn manifest(dir: &std::path::Path, storage: u16, hooks: &str) -> std::path::PathBuf {
    let set = concat!(env!("CARGO_MANIFEST_DIR"), "/src/sdk_descriptors.binpb");
    let m = format!(
        r#"{{"format": 1, "domain": "t", "env_prefix": "T", "language": "rust", "descriptor_set": "{set}",
            "register": {{"path": "t"}},
            "services": [{{"name": "w17.common.files.W17Files"}}],
            "storage": {{"address_env": "T_STORAGE", "default_address": "127.0.0.1:{storage}", "services": [],
                         "distributed_transactions": {{"service": "w17.common.distx.W17DistributedTransaction"}}}},
            "plugin_hooks": {hooks}}}"#
    );
    let p = dir.join("w17-business.json");
    std::fs::write(&p, m).unwrap();
    p
}

async fn coordinator() -> u16 {
    let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    tokio::spawn(
        tonic::transport::Server::builder()
            .layer(tower::util::MapResponseLayer::new(status_after_headers))
            .add_service(W17DistributedTransactionServer::new(Coordinator))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(l)),
    );
    port
}

/// With [`STATUS_AFTER_HEADERS`], a trailers-only error response becomes
/// response headers, then the status in the trailers.
fn status_after_headers(
    resp: http::Response<tonic::body::Body>,
) -> http::Response<tonic::body::Body> {
    if !STATUS_AFTER_HEADERS.load(Ordering::SeqCst) || resp.headers().get("grpc-status").is_none() {
        return resp;
    }
    let (mut parts, _) = resp.into_parts();
    let mut trailers = http::HeaderMap::new();
    for k in ["grpc-status", "grpc-message"] {
        if let Some(v) = parts.headers.remove(k) {
            trailers.insert(k, v);
        }
    }
    let frames = futures::stream::iter([Ok::<_, Status>(http_body::Frame::trailers(trailers))]);
    http::Response::from_parts(
        parts,
        tonic::body::Body::new(http_body_util::StreamBody::new(frames)),
    )
}

struct Tier {
    port: u16,
    stop: Option<oneshot::Sender<()>>,
    done: tokio::task::JoinHandle<Result<(), BootError>>,
    _dir: tempfile::TempDir,
}

async fn tier() -> Tier {
    let storage = coordinator().await;
    let dir = tempfile::tempdir().unwrap();
    let m = manifest(dir.path(), storage, "[]");
    let port = free_port();
    let env: Env = HashMap::from([("T_LISTEN_ADDR".into(), format!("127.0.0.1:{port}"))]);
    let (tx, rx) = oneshot::channel::<()>();
    let done = tokio::spawn(async move {
        serve_for_test(&m, env, "", register, Duration::from_secs(1), async {
            let _ = rx.await;
        })
        .await
    });
    let addr = format!("127.0.0.1:{port}");
    for _ in 0..100 {
        if probe(&addr, &Env::new(), Duration::from_millis(200))
            .await
            .is_ok()
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    Tier {
        port,
        stop: Some(tx),
        done,
        _dir: dir,
    }
}

impl Tier {
    async fn client(&self) -> W17FilesClient<tonic::transport::Channel> {
        let ch = tonic::transport::Endpoint::from_shared(format!("http://127.0.0.1:{}", self.port))
            .unwrap();
        W17FilesClient::new(ch.connect().await.unwrap())
    }

    async fn stop(mut self) {
        let _ = self.stop.take().unwrap().send(());
        self.done.await.unwrap().unwrap();
    }
}

fn stat(handle: &str) -> Request<StatRequest> {
    let mut r = Request::new(StatRequest {
        connection: String::new(),
        handle: handle.into(),
    });
    r.metadata_mut().insert("x-custom", "c".parse().unwrap());
    r.set_timeout(Duration::from_secs(5));
    r
}

/// One unary call sent as raw HTTP/2: the response headers and trailers as
/// the server wrote them (a tonic client folds them into one status).
async fn raw_unary<M: prost::Message>(
    port: u16,
    path: &str,
    req: Request<M>,
) -> (http::HeaderMap, http::HeaderMap) {
    use http_body_util::BodyExt as _;
    use tower::ServiceExt as _;
    let msg = req.into_inner().encode_to_vec();
    let mut framed = vec![0u8];
    framed.extend_from_slice(&u32::try_from(msg.len()).unwrap().to_be_bytes());
    framed.extend_from_slice(&msg);
    let ch = tonic::transport::Endpoint::from_shared(format!("http://127.0.0.1:{port}"))
        .unwrap()
        .connect()
        .await
        .unwrap();
    let http_req = http::Request::builder()
        .method("POST")
        .uri(format!("http://127.0.0.1:{port}{path}"))
        .header("content-type", "application/grpc")
        .header("te", "trailers")
        .body(tonic::body::Body::new(http_body_util::Full::new(
            bytes::Bytes::from(framed),
        )))
        .unwrap();
    let resp = ch.oneshot(http_req).await.unwrap();
    let (parts, body) = resp.into_parts();
    let trailers = body
        .collect()
        .await
        .unwrap()
        .trailers()
        .cloned()
        .unwrap_or_default();
    (parts.headers, trailers)
}

fn delete(handle: &str) -> Request<DeleteRequest> {
    Request::new(DeleteRequest {
        connection: String::new(),
        handle: handle.into(),
    })
}

// One tier serves every case: the statics above are process-wide.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_tier_over_the_wire() {
    let t = tier().await;
    let mut c = t.client().await;

    // The context: incoming metadata and the deadline.
    assert_eq!(c.stat(stat("ctx")).await.unwrap().into_inner().size, 11);
    // Composition: the caller's context travels unchanged.
    assert_eq!(c.stat(stat("self")).await.unwrap().into_inner().size, 11);
    let e = c.stat(stat("self-no-ctx")).await.unwrap_err();
    assert_eq!(e.code(), Code::FailedPrecondition, "{e:?}");
    // A channel refuses a call without a context.
    let e = c.stat(stat("no-ctx")).await.unwrap_err();
    assert!(
        e.code() == Code::FailedPrecondition && e.message().contains("ctx.request"),
        "{e:?}"
    );

    // A panic is INTERNAL, its text kept from the caller.
    let e = c.stat(stat("panic")).await.unwrap_err();
    assert_eq!(
        (e.code(), e.message()),
        (
            Code::Internal,
            "/w17.common.files.W17Files/Stat: internal error"
        )
    );
    // A handler's own UNAVAILABLE goes out after response headers carrying
    // `w17-answered`, so a w17 client (which replays a trailers-only
    // UNAVAILABLE) does not run the handler again.
    let (headers, trailers) = raw_unary(
        t.port,
        "/w17.common.files.W17Files/Stat",
        stat("unavailable"),
    )
    .await;
    assert!(
        headers.get("grpc-status").is_none(),
        "trailers-only: {headers:?}"
    );
    assert_eq!(
        headers.get("w17-answered").map(|v| v.as_bytes()),
        Some(&b"1"[..]),
        "{headers:?}"
    );
    assert_eq!(
        trailers.get("grpc-status").map(|v| v.as_bytes()),
        Some(&b"14"[..]),
        "{trailers:?}"
    );
    // for_user leaves prefixed with the method, the detail kept.
    let e = c.stat(stat("user")).await.unwrap_err();
    assert_eq!(
        (e.code(), e.message()),
        (Code::FailedPrecondition, "W17Files.Stat: dev text")
    );
    assert_eq!(crate::error_detail(&e).unwrap().code, "LOCKED");
    assert!(e.metadata().get(crate::errors::FOR_USER_MARKER).is_none());

    // The same two once a server stream has started writing: the panic is
    // INTERNAL in the trailers (and the connection survives it), the
    // for_user status prefixed, its marker kept off the wire.
    let mid = |handle: &str| {
        let mut r = Request::new(OpenRequest {
            connection: String::new(),
            handle: handle.into(),
        });
        r.set_timeout(Duration::from_secs(5));
        r
    };
    for (handle, want) in [
        (
            "panic",
            (
                Code::Internal,
                "/w17.common.files.W17Files/Open: internal error",
            ),
        ),
        (
            "user",
            (Code::FailedPrecondition, "W17Files.Open: dev text"),
        ),
    ] {
        let mut s = c.open(mid(handle)).await.unwrap().into_inner();
        assert_eq!(s.next().await.unwrap().unwrap().chunk, vec![1]);
        let e = s.next().await.unwrap().unwrap_err();
        assert_eq!((e.code(), e.message()), want, "{handle}: {e:?}");
        assert!(
            e.metadata().get(crate::errors::FOR_USER_MARKER).is_none(),
            "{handle}: {e:?}"
        );
        if handle == "user" {
            assert_eq!(crate::error_detail(&e).unwrap().code, "GONE");
        }
    }
    assert_eq!(c.stat(stat("ctx")).await.unwrap().into_inner().size, 11);

    // A client stream: served with its context, and ended by its deadline
    // when the stream never finishes.
    let chunk = |b: &[u8]| PutRequest {
        part: Some(crate::pb::w17::common::files::put_request::Part::Chunk(
            b.to_vec(),
        )),
    };
    let mut put = Request::new(futures::stream::iter(vec![chunk(b"ab"), chunk(b"cde")]));
    put.metadata_mut().insert("x-custom", "c".parse().unwrap());
    assert_eq!(c.put(put).await.unwrap().into_inner().handle, "5 true");
    let mut stalled =
        Request::new(futures::stream::iter(vec![chunk(b"ab")]).chain(futures::stream::pending()));
    stalled.set_timeout(Duration::from_millis(200));
    let start = Instant::now();
    let code = c.put(stalled).await.unwrap_err().code();
    assert!(
        matches!(code, Code::DeadlineExceeded | Code::Cancelled),
        "{code:?}"
    );
    assert!(start.elapsed() < Duration::from_secs(2));

    // The caller's deadline ends the call.
    let mut short = stat("sleep");
    short.set_timeout(Duration::from_millis(200));
    let start = Instant::now();
    // (tonic's own client ends the call at the same deadline, as CANCELLED:
    // the conformance suite's Go client asserts the server's code exactly.)
    let code = c.stat(short).await.unwrap_err().code();
    assert!(
        matches!(code, Code::DeadlineExceeded | Code::Cancelled),
        "{code:?}"
    );
    assert!(start.elapsed() < Duration::from_secs(2));
    // ... and a server stream's.
    let mut open = Request::new(OpenRequest::default());
    open.set_timeout(Duration::from_millis(350));
    let mut s = c.open(open).await.unwrap().into_inner();
    let mut got = 0;
    let end = loop {
        match s.next().await {
            Some(Ok(_)) => got += 1,
            Some(Err(e)) => break e.code(),
            None => break Code::Ok,
        }
    };
    assert!(
        matches!(end, Code::DeadlineExceeded | Code::Cancelled) && got < 10,
        "{end:?} after {got}"
    );

    // A transaction: one begin (the nested run joined), one commit, the
    // hook after it.
    CALLS.lock().unwrap().clear();
    let hooks = HOOK_RAN.load(Ordering::SeqCst);
    c.delete(delete("ok")).await.unwrap();
    assert_eq!(calls(), ["begin main", "commit tx-main tx-main"]);
    assert_eq!(HOOK_RAN.load(Ordering::SeqCst), hooks + 1);
    // An error rolls it back, the hook dropped, the error returned as it is.
    CALLS.lock().unwrap().clear();
    let e = c.delete(delete("fail")).await.unwrap_err();
    assert_eq!(
        (e.code(), e.message()),
        (Code::Aborted, "refused after the writes")
    );
    assert_eq!(calls(), ["begin main", "rollback tx-main"]);
    assert_eq!(HOOK_RAN.load(Ordering::SeqCst), hooks + 1);
    // A panicking hook is one reported failure: the transaction committed,
    // so the call succeeds, the hooks after it still run, and nothing of the
    // panic reaches the caller.
    CALLS.lock().unwrap().clear();
    let hooks = HOOK_RAN.load(Ordering::SeqCst);
    let r = c.delete(delete("panic-hook")).await;
    assert_eq!(calls(), ["begin main", "commit tx-main tx-main"]);
    assert!(r.is_ok(), "a committed call must answer OK: {r:?}");
    assert_eq!(
        HOOK_RAN.load(Ordering::SeqCst),
        hooks + 1,
        "the hook parked after the panicking one ran"
    );
    // A handler whose caller hangs up (its deadline passed) is dropped
    // mid-transaction: the drop guard rolls the transaction back, detached,
    // and the parked hook never runs.
    CALLS.lock().unwrap().clear();
    let mut hang = delete("hang");
    hang.set_timeout(Duration::from_millis(200));
    let code = c.delete(hang).await.unwrap_err().code();
    assert!(
        matches!(code, Code::DeadlineExceeded | Code::Cancelled),
        "{code:?}"
    );
    let start = Instant::now();
    while calls().last().map(String::as_str) != Some("rollback tx-main")
        && start.elapsed() < Duration::from_secs(2)
    {
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(calls(), ["begin main", "rollback tx-main"]);
    assert_eq!(HOOK_RAN.load(Ordering::SeqCst), hooks + 1);
    // A begin on another connection cannot join.
    let e = c.delete(delete("mismatch")).await.unwrap_err();
    assert!(
        e.code() == Code::Unknown && e.message().contains("cannot join"),
        "{e:?}"
    );
    // A commit lost in transport: retried as UNAVAILABLE is, then probed;
    // the probe finds the transaction gone, so the outcome is unknown.
    CALLS.lock().unwrap().clear();
    COMMIT_UNAVAILABLE.store(true, Ordering::SeqCst);
    let e = c.delete(delete("ok")).await.unwrap_err();
    COMMIT_UNAVAILABLE.store(false, Ordering::SeqCst);
    assert!(e.message().contains("outcome is unknown"), "{e:?}");
    let commits = calls().iter().filter(|c| c.starts_with("commit")).count();
    assert_eq!(commits, 3, "{:?}", calls());
    assert_eq!(calls().last().map(String::as_str), Some("rollback tx-main"));
    assert_eq!(
        HOOK_RAN.load(Ordering::SeqCst),
        hooks + 1,
        "work parked on an unknown outcome does not run"
    );
    // The same UNAVAILABLE after response headers: the server answered, so
    // the commit is never sent again (gRPC A6) — one commit, then the probe.
    CALLS.lock().unwrap().clear();
    COMMIT_UNAVAILABLE.store(true, Ordering::SeqCst);
    STATUS_AFTER_HEADERS.store(true, Ordering::SeqCst);
    let e = c.delete(delete("ok")).await.unwrap_err();
    COMMIT_UNAVAILABLE.store(false, Ordering::SeqCst);
    STATUS_AFTER_HEADERS.store(false, Ordering::SeqCst);
    assert!(e.message().contains("outcome is unknown"), "{e:?}");
    assert_eq!(
        calls(),
        ["begin main", "commit tx-main tx-main", "rollback tx-main"]
    );

    // Health answers SERVING; the drain ends the server.
    probe(
        &format!("127.0.0.1:{}", t.port),
        &Env::new(),
        Duration::from_secs(1),
    )
    .await
    .unwrap();
    let port = t.port;
    t.stop().await;
    assert!(
        probe(
            &format!("127.0.0.1:{port}"),
            &Env::new(),
            Duration::from_millis(300)
        )
        .await
        .is_err()
    );
}

#[tokio::test]
async fn the_boot_refuses() {
    let dir = tempfile::tempdir().unwrap();
    let m = manifest(dir.path(), 1, "[]");
    let never = async {};
    let e = serve_for_test(
        &m,
        Env::new(),
        "deadbeef",
        register,
        Duration::from_secs(1),
        never,
    )
    .await
    .unwrap_err();
    assert!(
        e.0.contains("is not the descriptor set this binary was compiled from")
            && e.0.contains("deadbeef"),
        "{e}"
    );
    let e = serve_for_test(
        &m,
        Env::new(),
        "",
        register_twice,
        Duration::from_secs(1),
        async {},
    )
    .await
    .unwrap_err();
    assert!(e.0.contains("already registered"), "{e}");
    let hooks = r#"[{"plugin": "p", "registered_as": "r", "slot": "s", "methods": ["/w17.common.files.W17Files/Gone"]}]"#;
    let m = manifest(dir.path(), 1, hooks);
    let e = serve_for_test(
        &m,
        Env::new(),
        "",
        register,
        Duration::from_secs(1),
        async {},
    )
    .await
    .unwrap_err();
    assert!(e.0.contains("has no handler"), "{e}");
}

// Go's GracefulStop: the drain sends GOAWAY first, so the client's next call
// goes elsewhere instead of being answered UNAVAILABLE on this connection,
// while the call in flight finishes. (Only stat cases, which touch none of
// the statics the other wire test counts.)
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_drain_sends_goaway_and_lets_the_call_in_flight_finish() {
    let mut t = tier().await;
    let c = t.client().await;
    let mut slow = c.clone();
    let mut req = stat("sleep");
    req.set_timeout(Duration::from_secs(20));
    let in_flight = tokio::spawn(async move { slow.stat(req).await });
    tokio::time::sleep(Duration::from_millis(200)).await;
    let _ = t.stop.take().unwrap().send(());
    tokio::time::sleep(Duration::from_millis(300)).await;
    let e = c.clone().stat(stat("ctx")).await.unwrap_err();
    assert!(
        !e.message().contains("server is shutting down"),
        "a call sent after the drain began still reached this connection: {e:?}"
    );
    let r = in_flight.await.unwrap().unwrap();
    assert_eq!(r.into_inner().size, 0, "the call in flight finished");
    t.done.await.unwrap().unwrap();
}

#[tokio::test]
async fn a_service_the_manifest_does_not_list_is_refused() {
    let pool =
        prost_reflect::DescriptorPool::decode(&include_bytes!("../sdk_descriptors.binpb")[..])
            .unwrap();
    let mut r = Registry::new(pool, &["w17.common.files.W17Files".to_string()], &[], &[]);
    let e = r
        .register(W17DistributedTransactionServer::new(Coordinator))
        .unwrap_err();
    assert!(e.0.contains("not a business service of this tier"), "{e}");
}

// An emitting method reads its whole request before the handler runs (the
// event is built from it). That read is part of the call: a body that never
// ends is cut off by the caller's deadline, as the handler would be.
struct Idle;

#[tonic::async_trait]
impl W17Files for Idle {
    async fn put(
        &self,
        _req: Request<tonic::Streaming<PutRequest>>,
    ) -> Result<Response<PutResponse>, Status> {
        Err(Status::unimplemented("put"))
    }

    type OpenStream = futures::stream::BoxStream<'static, Result<OpenResponse, Status>>;

    async fn open(&self, _req: Request<OpenRequest>) -> Result<Response<Self::OpenStream>, Status> {
        Err(Status::unimplemented("open"))
    }

    async fn stat(&self, _req: Request<StatRequest>) -> Result<Response<StatResponse>, Status> {
        Ok(Response::new(StatResponse::default()))
    }

    async fn delete(
        &self,
        _req: Request<DeleteRequest>,
    ) -> Result<Response<DeleteResponse>, Status> {
        Err(Status::unimplemented("delete"))
    }
}

#[tokio::test]
async fn an_emitting_methods_request_body_is_bound_by_the_deadline() {
    let pool =
        prost_reflect::DescriptorPool::decode(&include_bytes!("../sdk_descriptors.binpb")[..])
            .unwrap();
    let inv = crate::manifest::Invalidate {
        method: "/w17.common.files.W17Files/Stat".into(),
        channel: "c".into(),
        topic: "w17.invalidate".into(),
        group: "g".into(),
        entities: vec!["e".into()],
    };
    let mut r = Registry::new(
        pool,
        &["w17.common.files.W17Files".to_string()],
        &[],
        &[inv],
    );
    r.register(W17FilesServer::new(Idle)).unwrap();
    let served = r.finish(None, |routes| routes);
    // A request whose body never arrives.
    let body = http_body_util::StreamBody::new(futures::stream::pending::<
        Result<http_body::Frame<bytes::Bytes>, std::convert::Infallible>,
    >());
    let req = http::Request::builder()
        .method("POST")
        .uri("/w17.common.files.W17Files/Stat")
        .header("content-type", "application/grpc")
        .header("te", "trailers")
        .header("grpc-timeout", "100m")
        .body(body)
        .unwrap();
    let resp = tokio::time::timeout(Duration::from_secs(2), served.serve_wire(req))
        .await
        .expect("the deadline ends the read of the request");
    let st = Status::from_header_map(resp.headers()).unwrap();
    assert_eq!(st.code(), Code::DeadlineExceeded, "{st:?}");
}
