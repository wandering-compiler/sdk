//! A business method reached OVER THE WIRE inside another process's
//! transaction — the request carries the caller's `w17-tx-id`, and nothing in
//! this process opened it — holds its events until that transaction commits,
//! as it would inside one process: published once on the commit, never on a
//! rollback, and not twice when a commit refused while a call still held the
//! transaction is retried (Go's `distx.RemoteCommits`).
//!
//! The method is served through the middleware (`serve_wire`) with an
//! invalidate declared, in front of a scripted coordinator serving
//! `W17TransactionOutcome.Await`; the test decides when each transaction ends.

use std::{
    collections::{HashMap, HashSet},
    sync::{Arc, Mutex},
    time::Duration,
};

use futures::{StreamExt as _, channel::mpsc};
use http_body_util::BodyExt as _;
use prost::Message as _;
use tonic::{Request, Response, Status};

use crate::{
    Registry,
    channel::{Outbound, StorageChannel},
    distx::RemoteCommits,
    events::{Dispatcher, MemoryPublisher},
    observability::REPORTED,
    pb::w17::common::{
        distx::{
            AwaitRequest, AwaitResponse, TxOutcome,
            w17_transaction_outcome_server::{W17TransactionOutcome, W17TransactionOutcomeServer},
        },
        files::{
            DeleteRequest, DeleteResponse, OpenRequest, OpenResponse, PutRequest, PutResponse,
            StatRequest, StatResponse,
            w17_files_server::{W17Files, W17FilesServer},
        },
    },
    server::Served,
};

// --- the scripted coordinator -----------------------------------------------------

type Sender = mpsc::UnboundedSender<Result<AwaitResponse, Status>>;

/// How the coordinator answers an Await, by the tx id's prefix:
/// `open-*` PENDING then whatever the test sends; `stall-*` nothing until the
/// test sends; `committed-*` / `rolledback-*` the last message alone;
/// `unknown-*` NotFound.
#[derive(Default)]
struct Outcomes {
    open: Mutex<HashMap<String, Sender>>,
    /// Every Await: its tx id and the metadata keys it carried.
    awaits: Mutex<Vec<(String, HashMap<String, String>)>>,
}

impl Outcomes {
    fn end(&self, tx: &str, outcome: TxOutcome) {
        let tx_sender = self.open.lock().unwrap().remove(tx).unwrap_or_else(|| {
            panic!("no wait is open for {tx}: the method never asked the coordinator")
        });
        tx_sender
            .unbounded_send(Ok(AwaitResponse {
                outcome: outcome as i32,
            }))
            .unwrap();
    }

    fn fail(&self, tx: &str, st: Status) {
        let tx_sender = self.open.lock().unwrap().remove(tx).unwrap_or_else(|| {
            panic!("no wait is open for {tx}: the method never asked the coordinator")
        });
        tx_sender.unbounded_send(Err(st)).unwrap();
    }

    fn awaits(&self) -> Vec<(String, HashMap<String, String>)> {
        self.awaits.lock().unwrap().clone()
    }
}

struct Fake(Arc<Outcomes>);

fn msg(o: TxOutcome) -> Result<AwaitResponse, Status> {
    Ok(AwaitResponse { outcome: o as i32 })
}

#[tonic::async_trait]
impl W17TransactionOutcome for Fake {
    type AwaitStream = futures::stream::BoxStream<'static, Result<AwaitResponse, Status>>;

    async fn r#await(
        &self,
        req: Request<AwaitRequest>,
    ) -> Result<Response<Self::AwaitStream>, Status> {
        let md: HashMap<String, String> = req
            .metadata()
            .clone()
            .into_headers()
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("?").to_string()))
            .collect();
        let tx = req.into_inner().tx_id;
        self.0.awaits.lock().unwrap().push((tx.clone(), md));
        let (send, recv) = mpsc::unbounded();
        if tx.starts_with("open-") {
            send.unbounded_send(msg(TxOutcome::Pending)).unwrap();
            self.0.open.lock().unwrap().insert(tx, send);
        } else if tx.starts_with("stall-") {
            self.0.open.lock().unwrap().insert(tx, send);
        } else if tx.starts_with("committed-") {
            send.unbounded_send(msg(TxOutcome::Committed)).unwrap();
        } else if tx.starts_with("rolledback-") {
            send.unbounded_send(msg(TxOutcome::NotCommitted)).unwrap();
        } else {
            return Err(Status::not_found(format!(
                "await: unknown transaction {tx:?}"
            )));
        }
        Ok(Response::new(recv.boxed()))
    }
}

/// A coordinator serving the outcome service (`serve_outcome`), or one that
/// predates it (it serves only health).
async fn coordinator(outcomes: Arc<Outcomes>, serve_outcome: bool) -> RemoteCommits {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (_, health) = tonic_health::server::health_reporter();
    let mut routes = tonic::service::Routes::new(health);
    if serve_outcome {
        routes = routes.add_service(W17TransactionOutcomeServer::new(Fake(outcomes)));
    }
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener)),
    );
    let ch = tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
        .unwrap()
        .connect_lazy();
    RemoteCommits::new(StorageChannel(Arc::new(Outbound::new(
        ch,
        false,
        HashSet::new(),
    ))))
}

// --- the business method ----------------------------------------------------------

/// `Stat` answers OK; its invalidate is the event under test.
struct Method;

#[tonic::async_trait]
impl W17Files for Method {
    async fn put(
        &self,
        _: Request<tonic::Streaming<PutRequest>>,
    ) -> Result<Response<PutResponse>, Status> {
        Err(Status::unimplemented("put"))
    }

    type OpenStream = futures::stream::BoxStream<'static, Result<OpenResponse, Status>>;

    async fn open(&self, _: Request<OpenRequest>) -> Result<Response<Self::OpenStream>, Status> {
        Err(Status::unimplemented("open"))
    }

    async fn stat(&self, _: Request<StatRequest>) -> Result<Response<StatResponse>, Status> {
        Ok(Response::new(StatResponse {
            size: 1,
            mod_time: None,
        }))
    }

    async fn delete(&self, _: Request<DeleteRequest>) -> Result<Response<DeleteResponse>, Status> {
        Err(Status::unimplemented("delete"))
    }
}

struct Tier {
    served: Served,
    published: Arc<MemoryPublisher>,
}

fn tier(remote: Option<RemoteCommits>) -> Tier {
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
    r.register(W17FilesServer::new(Method)).unwrap();
    let published = Arc::new(MemoryPublisher::default());
    let dispatcher: Arc<dyn Dispatcher> = published.clone();
    let served = r.finish(Some(dispatcher), remote, |routes| routes);
    Tier { served, published }
}

impl Tier {
    /// Call `Stat` as a caller in another process does inside its
    /// transaction `tx`; returns once the method answered.
    async fn call(&self, tx: &str, conn: Option<&str>) {
        let body = StatRequest {
            connection: String::new(),
            handle: "h".into(),
        }
        .encode_to_vec();
        let mut framed = vec![0u8];
        framed.extend_from_slice(&u32::try_from(body.len()).unwrap().to_be_bytes());
        framed.extend_from_slice(&body);
        let mut b = http::Request::builder()
            .method("POST")
            .uri("/w17.common.files.W17Files/Stat")
            .header("content-type", "application/grpc")
            .header("te", "trailers")
            .header("w17-tx-id", tx)
            // What the request carries besides: none of it is the wait's.
            .header("authorization", "Bearer secret")
            .header("x-w17-user-id", "u-1")
            .header("x-custom", "c");
        if let Some(c) = conn {
            b = b.header("w17-conn-id", c);
        }
        let req = b
            .body(http_body_util::Full::new(bytes::Bytes::from(framed)))
            .unwrap();
        let resp =
            tokio::time::timeout(Duration::from_secs(20), self.served.clone().serve_wire(req))
                .await
                .expect("the method answered");
        let trailers = resp
            .into_body()
            .collect()
            .await
            .unwrap()
            .trailers()
            .cloned()
            .unwrap_or_default();
        assert_eq!(
            trailers.get("grpc-status").map(|v| v.to_str().unwrap()),
            Some("0"),
            "{trailers:?}"
        );
    }

    fn count(&self) -> usize {
        self.published.published.lock().unwrap().len()
    }

    /// Wait until `n` events went out (or 3 s), then a little longer, so a
    /// duplicate has its chance to show.
    async fn settle(&self, n: usize) -> usize {
        let until = tokio::time::Instant::now() + Duration::from_secs(3);
        while self.count() < n && tokio::time::Instant::now() < until {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        tokio::time::sleep(Duration::from_millis(150)).await;
        self.count()
    }
}

/// The reports that name `tx`.
fn reports(tx: &str) -> Vec<String> {
    let q = format!("{tx:?}");
    REPORTED
        .lock()
        .unwrap()
        .iter()
        .filter(|r| r.contains(&q))
        .cloned()
        .collect()
}

// --- the cases --------------------------------------------------------------------

#[tokio::test]
async fn remote_emit_published_once_after_the_callers_commit() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("open-commit", Some("replica-7")).await;
    assert_eq!(
        t.settle(0).await,
        0,
        "the method announced before its caller's transaction committed"
    );
    o.end("open-commit", TxOutcome::Committed);
    assert_eq!(
        t.settle(1).await,
        1,
        "announced how many times after the commit"
    );
    assert_eq!(reports("open-commit"), Vec::<String>::new());
}

#[tokio::test]
async fn remote_emit_nothing_after_the_callers_rollback() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("open-rollback", None).await;
    o.end("open-rollback", TxOutcome::NotCommitted);
    assert_eq!(
        t.settle(1).await,
        0,
        "a rolled-back transaction's callee announced"
    );
    // An ordinary rollback is not an operator's problem.
    assert_eq!(reports("open-rollback"), Vec::<String>::new());
}

/// A Commit the coordinator refuses because a call inside the transaction
/// still holds it ends nothing, so it sends nothing; only the retried commit
/// that lands answers COMMITTED, once.
#[tokio::test]
async fn remote_emit_a_busy_commit_retried_is_announced_once() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("open-busy", None).await;
    // The refused commit: PENDING stands, nothing more for a while.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(t.count(), 0, "announced on a commit that did not happen");
    o.end("open-busy", TxOutcome::Committed);
    assert_eq!(
        t.settle(1).await,
        1,
        "announced how many times after the retry"
    );
}

#[tokio::test]
async fn remote_emit_a_coordinator_without_the_outcome_service_announces_now_and_says_so() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), false).await));
    t.call("open-old", None).await;
    assert_eq!(t.count(), 1, "not announced when the method answered");
    let r = reports("open-old");
    assert!(
        r.len() == 1
            && r[0].contains("before it commits: its coordinator does not serve W17TransactionOutcome (upgrade the storage bundle)"),
        "{r:?}"
    );
}

#[tokio::test]
async fn remote_emit_an_unknown_transaction_is_dropped_and_said() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("unknown-1", None).await;
    assert_eq!(
        t.settle(1).await,
        0,
        "announced inside a transaction nobody knows"
    );
    let r = reports("unknown-1");
    assert!(
        r.len() == 1
            && r[0].contains("dropped the deferred announcement of tx \"unknown-1\": its coordinator could not be asked how the transaction ends (NotFound"),
        "{r:?}"
    );
}

/// The wait reaches the transaction's replica: the request's `w17-conn-id`
/// goes along, and nothing else of the request does.
#[tokio::test]
async fn remote_emit_echoes_only_the_routing_token() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("open-route", Some("replica-7")).await;
    t.call("open-noroute", None).await;
    let awaits = o.awaits();
    let own = |md: &HashMap<String, String>| -> Vec<String> {
        let mut k: Vec<String> = md
            .keys()
            .filter(|k| {
                !matches!(
                    k.as_str(),
                    "te" | "content-type"
                        | "content-length"
                        | "user-agent"
                        | "grpc-accept-encoding"
                        | "grpc-encoding"
                )
            })
            .cloned()
            .collect();
        k.sort();
        k
    };
    assert_eq!(awaits.len(), 2, "{awaits:?}");
    assert_eq!(awaits[0].0, "open-route");
    assert_eq!(own(&awaits[0].1), ["w17-conn-id"], "{:?}", awaits[0].1);
    assert_eq!(awaits[0].1["w17-conn-id"], "replica-7");
    assert_eq!(own(&awaits[1].1), Vec::<String>::new(), "{:?}", awaits[1].1);
    o.end("open-route", TxOutcome::Committed);
    o.end("open-noroute", TxOutcome::NotCommitted);
    assert_eq!(t.settle(1).await, 1);
}

#[tokio::test]
async fn remote_emit_a_transaction_that_already_ended() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("committed-1", None).await;
    assert_eq!(
        t.count(),
        1,
        "a committed transaction's event waits for nothing"
    );
    t.call("rolledback-1", None).await;
    assert_eq!(
        t.settle(1).await,
        1,
        "an event of a rolled-back transaction went out"
    );
    let r = reports("rolledback-1");
    assert!(
        r.len() == 1
            && r[0].contains("the transaction ended without committing before the emit registered"),
        "{r:?}"
    );
}

#[tokio::test]
async fn remote_emit_a_coordinator_that_does_not_confirm_in_time_drops_it() {
    let o = Arc::new(Outcomes::default());
    let remote = coordinator(o.clone(), true)
        .await
        .with_register_wait(Duration::from_millis(200));
    let t = tier(Some(remote));
    t.call("stall-1", None).await;
    let r = reports("stall-1");
    assert!(
        r.len() == 1 && r[0].contains("could not be asked how the transaction ends (the coordinator did not confirm the wait within 200ms)"),
        "{r:?}"
    );
    // A late answer reaches nobody.
    o.end("stall-1", TxOutcome::Committed);
    assert_eq!(t.settle(1).await, 0);
}

#[tokio::test]
async fn remote_emit_a_coordinator_that_stops_answering_drops_it() {
    let o = Arc::new(Outcomes::default());
    let t = tier(Some(coordinator(o.clone(), true).await));
    t.call("open-cut", None).await;
    o.fail("open-cut", Status::unavailable("replica gone"));
    assert_eq!(t.settle(1).await, 0);
    let until = tokio::time::Instant::now() + Duration::from_secs(3);
    while reports("open-cut").is_empty() && tokio::time::Instant::now() < until {
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let r = reports("open-cut");
    assert!(
        r.len() == 1
            && r[0].contains("the coordinator stopped answering before the transaction ended (Unavailable: replica gone), so whether it committed is unknown"),
        "{r:?}"
    );
}

/// A tier whose storage runs no distributed transactions has nobody to ask:
/// the event goes out at once, and nothing is reported.
#[tokio::test]
async fn remote_emit_without_a_coordinator_announces_now() {
    let t = tier(None);
    t.call("open-nocoord", None).await;
    assert_eq!(t.count(), 1);
    assert_eq!(reports("open-nocoord"), Vec::<String>::new());
}

/// The shutdown drain gave up on a wait whose transaction is still open: the
/// runtime abandons it before it closes the dispatcher. The wait stops, says
/// that it dropped the event, and a commit landing afterwards publishes
/// nothing — it would publish on a dispatcher already closed.
#[tokio::test]
async fn remote_emit_a_wait_the_shutdown_abandons_stops_and_never_publishes() {
    let o = Arc::new(Outcomes::default());
    let remote = coordinator(o.clone(), true).await;
    let t = tier(Some(remote.clone()));
    t.call("open-abandoned", None).await;
    remote.abandon_waits();
    let until = tokio::time::Instant::now() + Duration::from_secs(3);
    while reports("open-abandoned").is_empty() && tokio::time::Instant::now() < until {
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let r = reports("open-abandoned");
    assert!(
        r.len() == 1
            && r[0]
                .contains("(the shutdown abandoned the wait), so whether it committed is unknown"),
        "{r:?}"
    );
    // The commit lands after the shutdown gave up: nobody listens any more.
    if let Some(send) = o.open.lock().unwrap().remove("open-abandoned") {
        let _ = send.unbounded_send(msg(TxOutcome::Committed));
    }
    assert_eq!(t.settle(1).await, 0);
}
