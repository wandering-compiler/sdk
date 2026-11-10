//! The proxy on the transaction path when something goes wrong: an affinity
//! entry that ages while its transaction is busy, a Commit the replica
//! answers UNAVAILABLE, a response whose status rides the trailers, and a
//! pinned replica that goes away mid-transaction.

use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use http::{HeaderMap, Request, Response};
use http_body_util::{BodyExt, Empty, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto::Builder as ConnBuilder;
use tokio::net::TcpListener;
use tokio::task::JoinHandle;

use storage_proxy::config::Config;
use storage_proxy::server;

const BEGIN: &str = "/proj.storage/w17.common.distx.W17DistributedTransaction/Begin";
const COMMIT: &str = "/proj.storage/w17.common.distx.W17DistributedTransaction/Commit";
const ROLLBACK: &str = "/proj.storage/w17.common.distx.W17DistributedTransaction/Rollback";
const UPDATE: &str = "/proj.notes.NotesMutation/Update";

type BoxError = Box<dyn std::error::Error + Send + Sync>;

/// How a fake replica answers one request.
#[derive(Clone)]
enum Reply {
    /// A trailers-only response with this `grpc-status`.
    TrailersOnly(&'static str),
    /// Response headers, one data frame, then trailers with `grpc-status: 0`
    /// and a `grpc-status-details-bin`.
    WithTrailers,
}

type Behaviour = Arc<dyn Fn(&str) -> Reply + Send + Sync>;

/// A fake storage replica: records every path it served, answers per
/// `behaviour`, and can be killed (listener and open connections closed).
struct Replica {
    addr: String,
    hits: Arc<Mutex<Vec<String>>>,
    tasks: Arc<Mutex<Vec<JoinHandle<()>>>>,
}

impl Replica {
    async fn start(behaviour: Behaviour) -> Replica {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let hits = Arc::new(Mutex::new(Vec::new()));
        let tasks: Arc<Mutex<Vec<JoinHandle<()>>>> = Arc::new(Mutex::new(Vec::new()));
        let (h, t) = (hits.clone(), tasks.clone());
        let accept = tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                let io = TokioIo::new(stream);
                let (hits, behaviour) = (h.clone(), behaviour.clone());
                let conn = tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let path = req.uri().path().to_string();
                        hits.lock().unwrap().push(path.clone());
                        let reply = behaviour(&path);
                        async move { Ok::<_, Infallible>(respond(reply)) }
                    });
                    let _ = ConnBuilder::new(TokioExecutor::new())
                        .serve_connection(io, svc)
                        .await;
                });
                t.lock().unwrap().push(conn);
            }
        });
        tasks.lock().unwrap().push(accept);
        Replica { addr, hits, tasks }
    }

    fn hits(&self) -> Vec<String> {
        self.hits.lock().unwrap().clone()
    }

    /// Stop accepting and drop every open connection.
    fn kill(&self) {
        for t in self.tasks.lock().unwrap().drain(..) {
            t.abort();
        }
    }
}

fn respond(reply: Reply) -> Response<http_body_util::combinators::BoxBody<Bytes, BoxError>> {
    let builder = Response::builder()
        .status(200)
        .header("content-type", "application/grpc");
    match reply {
        Reply::TrailersOnly(code) => builder
            .header("grpc-status", code)
            .body(
                Empty::<Bytes>::new()
                    .map_err(|e: Infallible| -> BoxError { match e {} })
                    .boxed(),
            )
            .unwrap(),
        Reply::WithTrailers => {
            let mut trailers = HeaderMap::new();
            trailers.insert("grpc-status", "0".parse().unwrap());
            trailers.insert("grpc-status-details-bin", "AAEC".parse().unwrap());
            let frames = futures_frames(vec![
                Frame::data(Bytes::from_static(b"\0\0\0\0\x02hi")),
                Frame::trailers(trailers),
            ]);
            builder.body(StreamBody::new(frames).boxed()).unwrap()
        }
    }
}

fn futures_frames(
    frames: Vec<Frame<Bytes>>,
) -> impl tokio_stream::Stream<Item = Result<Frame<Bytes>, BoxError>> + Send + Sync + 'static {
    tokio_stream::iter(frames.into_iter().map(Ok))
}

/// What one call through the proxy answered.
struct Answer {
    conn_id: Option<String>,
    /// `grpc-status` from the leading headers (trailers-only), else from the
    /// trailers.
    grpc_status: Option<String>,
    trailers: HeaderMap,
}

async fn call(proxy: &str, path: &str, conn_id: Option<&str>) -> Answer {
    let client: hyper_util::client::legacy::Client<_, Empty<Bytes>> =
        hyper_util::client::legacy::Client::builder(TokioExecutor::new())
            .http2_only(true)
            .build(hyper_util::client::legacy::connect::HttpConnector::new());
    let mut b = Request::builder()
        .uri(format!("http://{proxy}{path}"))
        .method("POST")
        .header("content-type", "application/grpc");
    if let Some(id) = conn_id {
        b = b.header("w17-conn-id", id);
    }
    let resp = client.request(b.body(Empty::new()).unwrap()).await.unwrap();
    let header = |h: &HeaderMap, k: &str| h.get(k).and_then(|v| v.to_str().ok()).map(String::from);
    let conn_id = header(resp.headers(), "w17-conn-id");
    let leading = header(resp.headers(), "grpc-status");
    let trailers = resp
        .into_body()
        .collect()
        .await
        .unwrap()
        .trailers()
        .cloned()
        .unwrap_or_default();
    let grpc_status = leading.or_else(|| header(&trailers, "grpc-status"));
    Answer {
        conn_id,
        grpc_status,
        trailers,
    }
}

async fn proxy_in_front_of(replicas: Vec<String>, ttl: Duration) -> String {
    let listen = TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap();
    tokio::spawn(server::run(Config {
        listen,
        replicas,
        affinity_ttl: ttl,
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        server_tls: None,
        backend_tls: None,
    }));
    tokio::time::sleep(Duration::from_millis(150)).await;
    listen.to_string()
}

fn ok_everywhere() -> Behaviour {
    Arc::new(|_| Reply::TrailersOnly("0"))
}

/// The affinity TTL is an IDLE timeout: a transaction that keeps calling
/// keeps its route past the TTL (its Rollback still reaches the replica),
/// and one left idle past it loses the route.
#[tokio::test]
async fn ttl_counts_from_the_last_call_not_from_begin() {
    let r = Replica::start(ok_everywhere()).await;
    let proxy = proxy_in_front_of(vec![r.addr.clone()], Duration::from_secs(1)).await;

    let id = call(&proxy, BEGIN, None).await.conn_id.expect("stamped");
    for _ in 0..4 {
        tokio::time::sleep(Duration::from_millis(400)).await;
        let a = call(&proxy, UPDATE, Some(&id)).await;
        assert_eq!(
            a.grpc_status.as_deref(),
            Some("0"),
            "a busy transaction keeps its route"
        );
    }
    let a = call(&proxy, ROLLBACK, Some(&id)).await;
    assert_eq!(a.grpc_status.as_deref(), Some("0"));
    assert_eq!(r.hits().len(), 6, "{:?}", r.hits());

    // Idle past the TTL: the route is gone and nothing reaches the replica.
    let id = call(&proxy, BEGIN, None).await.conn_id.expect("stamped");
    tokio::time::sleep(Duration::from_millis(1200)).await;
    let a = call(&proxy, UPDATE, Some(&id)).await;
    assert_eq!(a.grpc_status.as_deref(), Some("9"));
    assert_eq!(r.hits().len(), 7);
}

/// A Commit the replica answers with a trailers-only UNAVAILABLE did not
/// finish the transaction (the replica refused it, e.g. while draining), so
/// the route stays: the caller's retry or its settle Rollback, carrying the
/// same conn-id, reaches the pinned replica. Any other answer finishes it.
#[tokio::test]
async fn an_unavailable_commit_keeps_the_route() {
    let r = Replica::start(Arc::new(|p: &str| {
        if p.ends_with("/Commit") {
            Reply::TrailersOnly("14")
        } else {
            Reply::TrailersOnly("0")
        }
    }))
    .await;
    let proxy = proxy_in_front_of(vec![r.addr.clone()], Duration::from_secs(300)).await;

    let id = call(&proxy, BEGIN, None).await.conn_id.expect("stamped");
    let a = call(&proxy, COMMIT, Some(&id)).await;
    assert_eq!(
        a.grpc_status.as_deref(),
        Some("14"),
        "the replica's answer is relayed"
    );
    let a = call(&proxy, ROLLBACK, Some(&id)).await;
    assert_eq!(
        a.grpc_status.as_deref(),
        Some("0"),
        "the settle Rollback reached the replica"
    );
    assert_eq!(r.hits(), [BEGIN, COMMIT, ROLLBACK]);
    // The Rollback answered OK: the route is gone now.
    let a = call(&proxy, UPDATE, Some(&id)).await;
    assert_eq!(a.grpc_status.as_deref(), Some("9"));
    assert_eq!(r.hits().len(), 3);
}

/// A response whose status rides the trailers (headers, a message, then
/// `grpc-status` with `grpc-status-details-bin`) reaches the caller with its
/// trailers intact, on every class of call.
#[tokio::test]
async fn trailers_are_forwarded() {
    let r = Replica::start(Arc::new(|_| Reply::WithTrailers)).await;
    let proxy = proxy_in_front_of(vec![r.addr.clone()], Duration::from_secs(300)).await;

    let begin = call(&proxy, BEGIN, None).await;
    let id = begin.conn_id.clone().expect("stamped");
    for a in [
        begin,
        call(&proxy, UPDATE, Some(&id)).await,
        call(&proxy, COMMIT, Some(&id)).await,
        call(&proxy, "/proj.notes.NotesQuery/List", None).await,
    ] {
        assert_eq!(a.grpc_status.as_deref(), Some("0"));
        assert_eq!(
            a.trailers
                .get("grpc-status-details-bin")
                .map(|v| v.as_bytes()),
            Some(&b"AAEC"[..]),
            "{:?}",
            a.trailers
        );
    }
}

/// A pinned replica that goes away mid-transaction: the in-transaction call
/// is answered UNAVAILABLE and is NOT re-balanced onto another replica (the
/// open transaction lives only on the pinned one), and the route is kept.
#[tokio::test]
async fn a_dead_pinned_replica_is_unavailable_never_rebalanced() {
    let r0 = Replica::start(ok_everywhere()).await;
    let r1 = Replica::start(ok_everywhere()).await;
    let proxy = proxy_in_front_of(
        vec![r0.addr.clone(), r1.addr.clone()],
        Duration::from_secs(300),
    )
    .await;

    let id = call(&proxy, BEGIN, None).await.conn_id.expect("stamped");
    let (pinned, other) = if r0.hits().len() == 1 {
        (&r0, &r1)
    } else {
        (&r1, &r0)
    };
    pinned.kill();
    tokio::time::sleep(Duration::from_millis(100)).await;
    for path in [UPDATE, UPDATE, COMMIT] {
        let a = call(&proxy, path, Some(&id)).await;
        assert_eq!(a.grpc_status.as_deref(), Some("14"), "{path}");
    }
    assert!(other.hits().is_empty(), "re-balanced: {:?}", other.hits());
}

/// A Begin the replica refuses opened no transaction: the refusal is relayed
/// with no conn-id stamped, and its route is not kept for the TTL.
#[tokio::test]
async fn a_refused_begin_is_relayed_without_a_conn_id() {
    let r = Replica::start(Arc::new(|p: &str| {
        if p.ends_with("/Begin") {
            Reply::TrailersOnly("3")
        } else {
            Reply::TrailersOnly("0")
        }
    }))
    .await;
    let proxy = proxy_in_front_of(vec![r.addr.clone()], Duration::from_secs(300)).await;
    let a = call(&proxy, BEGIN, None).await;
    assert_eq!(a.grpc_status.as_deref(), Some("3"));
    assert_eq!(a.conn_id, None, "a refused Begin names no transaction");
}
