//! End-to-end integration test with fake in-process h2 upstreams.
//!
//! Spins up two fake "storage replicas" (each an h2 server that records
//! the path + `w17-conn-id` it received and replies with which replica
//! it is), runs the real proxy in front of them, and drives it with a
//! raw h2 client to assert the affinity contract:
//!
//!   1. `Begin` → proxy mints a `w17-conn-id` and stamps it into the
//!      response headers; the request lands on exactly one replica.
//!   2. A follow-up RPC carrying that `w17-conn-id` lands on the SAME
//!      replica (affinity beats round-robin).
//!   3. `Commit` routes to the pinned replica and evicts the entry; a
//!      further RPC with the now-evicted id is rejected with
//!      `grpc-status: 9` (FAILED_PRECONDITION) and never reaches a
//!      replica.

use std::convert::Infallible;
use std::sync::Arc;
use std::time::Duration;

use http::{Request, Response};
use http_body_util::{BodyExt, Empty, Full};
use hyper::body::{Bytes, Incoming};
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto::Builder as ConnBuilder;
use tokio::net::TcpListener;

use storage_proxy::config::Config;
use storage_proxy::server;

/// Records, per replica, the (path, conn_id) of every request it served.
#[derive(Default)]
struct Hits {
    records: std::sync::Mutex<Vec<(usize, String, Option<String>)>>,
}
impl Hits {
    fn record(&self, replica: usize, path: String, conn_id: Option<String>) {
        self.records.lock().unwrap().push((replica, path, conn_id));
    }
    fn for_replica(&self, replica: usize) -> usize {
        self.records
            .lock()
            .unwrap()
            .iter()
            .filter(|(r, ..)| *r == replica)
            .count()
    }
}

/// Start a fake replica h2 server; returns its `host:port`.
async fn start_replica(replica: usize, hits: Arc<Hits>) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let io = TokioIo::new(stream);
            let hits = hits.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let hits = hits.clone();
                    async move {
                        let path = req.uri().path().to_string();
                        let conn_id = req
                            .headers()
                            .get("w17-conn-id")
                            .and_then(|v| v.to_str().ok())
                            .map(|s| s.to_string());
                        hits.record(replica, path, conn_id);
                        // A minimal "trailers-only" gRPC OK response with
                        // a tiny body so the client can read it.
                        let body = Full::new(Bytes::from(format!("replica-{replica}")))
                            .map_err(
                                |e: Infallible| -> Box<dyn std::error::Error + Send + Sync> {
                                    match e {}
                                },
                            )
                            .boxed();
                        let resp = Response::builder()
                            .status(200)
                            .header("content-type", "application/grpc")
                            .header("grpc-status", "0")
                            .header("x-replica", replica.to_string())
                            .body(body)
                            .unwrap();
                        Ok::<_, Infallible>(resp)
                    }
                });
                let _ = ConnBuilder::new(TokioExecutor::new())
                    .serve_connection(io, svc)
                    .await;
            });
        }
    });
    format!("{}:{}", addr.ip(), addr.port())
}

/// Pick a free port for the proxy's listener.
async fn free_addr() -> std::net::SocketAddr {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    l.local_addr().unwrap()
}

/// A raw h2 client request to the proxy. Returns (status-code,
/// w17-conn-id response header, grpc-status header).
async fn call(
    proxy: &str,
    path: &str,
    conn_id: Option<&str>,
) -> (u16, Option<String>, Option<String>) {
    let client: hyper_util::client::legacy::Client<_, Empty<Bytes>> =
        hyper_util::client::legacy::Client::builder(TokioExecutor::new())
            .http2_only(true)
            .build(hyper_util::client::legacy::connect::HttpConnector::new());

    let mut builder = Request::builder()
        .uri(format!("http://{proxy}{path}"))
        .method("POST");
    builder = builder.header("content-type", "application/grpc");
    if let Some(id) = conn_id {
        builder = builder.header("w17-conn-id", id);
    }
    let req = builder.body(Empty::<Bytes>::new()).unwrap();
    let resp = client.request(req).await.unwrap();
    let status = resp.status().as_u16();
    let conn = resp
        .headers()
        .get("w17-conn-id")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string());
    let grpc = resp
        .headers()
        .get("grpc-status")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string());
    // drain body so the stream completes
    let _ = resp.into_body().collect().await;
    (status, conn, grpc)
}

const BEGIN: &str = "/proj.storage/w17.common.distx.W17DistributedTransaction/Begin";
const COMMIT: &str = "/proj.storage/w17.common.distx.W17DistributedTransaction/Commit";
const UPDATE: &str = "/proj.notes.NotesMutation/Update";

#[tokio::test]
async fn begin_pins_followup_routes_commit_evicts() {
    let hits = Arc::new(Hits::default());
    let r0 = start_replica(0, hits.clone()).await;
    let r1 = start_replica(1, hits.clone()).await;

    let listen = free_addr().await;
    let cfg = Config {
        listen,
        replicas: vec![r0, r1],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        // Routing/affinity integration runs over plain h2c (no TLS material).
        server_tls: None,
        backend_tls: None,
    };
    tokio::spawn(server::run(cfg));
    // give the proxy a moment to bind
    tokio::time::sleep(Duration::from_millis(150)).await;
    let proxy = listen.to_string();

    // 1. Begin → mints + stamps conn_id, lands on one replica.
    let (status, conn_id, grpc) = call(&proxy, BEGIN, None).await;
    assert_eq!(status, 200);
    assert_eq!(grpc.as_deref(), Some("0"));
    let conn_id = conn_id.expect("proxy must stamp w17-conn-id on Begin response");
    assert!(!conn_id.is_empty());
    let total_after_begin = hits.for_replica(0) + hits.for_replica(1);
    assert_eq!(total_after_begin, 1, "Begin must hit exactly one replica");
    let pinned = if hits.for_replica(0) == 1 { 0 } else { 1 };

    // 2. Follow-up with that conn_id → SAME replica.
    let (status, _, grpc) = call(&proxy, UPDATE, Some(&conn_id)).await;
    assert_eq!(status, 200);
    assert_eq!(grpc.as_deref(), Some("0"));
    assert_eq!(
        hits.for_replica(pinned),
        2,
        "in-tx RPC must hit the pinned replica (affinity)"
    );
    let other = 1 - pinned;
    assert_eq!(
        hits.for_replica(other),
        0,
        "in-tx RPC must not touch the other replica"
    );

    // 3. Commit → routes to pinned replica, then evicts.
    let (status, _, grpc) = call(&proxy, COMMIT, Some(&conn_id)).await;
    assert_eq!(status, 200);
    assert_eq!(grpc.as_deref(), Some("0"));
    assert_eq!(
        hits.for_replica(pinned),
        3,
        "Commit must hit the pinned replica"
    );

    // 3b. Re-using the evicted conn_id → rejected, no replica hit.
    let before = hits.for_replica(0) + hits.for_replica(1);
    let (status, _, grpc) = call(&proxy, UPDATE, Some(&conn_id)).await;
    assert_eq!(status, 200, "gRPC errors ride on HTTP 200");
    assert_eq!(
        grpc.as_deref(),
        Some("9"),
        "evicted conn_id => FAILED_PRECONDITION"
    );
    let after = hits.for_replica(0) + hits.for_replica(1);
    assert_eq!(after, before, "rejected RPC must not reach any replica");
}

#[tokio::test]
async fn non_tx_requests_round_robin_across_replicas() {
    let hits = Arc::new(Hits::default());
    let r0 = start_replica(0, hits.clone()).await;
    let r1 = start_replica(1, hits.clone()).await;

    let listen = free_addr().await;
    let cfg = Config {
        listen,
        replicas: vec![r0, r1],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        // Routing/affinity integration runs over plain h2c (no TLS material).
        server_tls: None,
        backend_tls: None,
    };
    tokio::spawn(server::run(cfg));
    tokio::time::sleep(Duration::from_millis(150)).await;
    let proxy = listen.to_string();

    // Four non-tx calls (no conn_id) should spread 2/2 across replicas.
    for _ in 0..4 {
        let (status, _, _) = call(&proxy, "/proj.notes.NotesQuery/List", None).await;
        assert_eq!(status, 200);
    }
    assert_eq!(hits.for_replica(0), 2);
    assert_eq!(hits.for_replica(1), 2);
}

#[tokio::test]
async fn unknown_conn_id_rejected_without_upstream() {
    let hits = Arc::new(Hits::default());
    let r0 = start_replica(0, hits.clone()).await;

    let listen = free_addr().await;
    let cfg = Config {
        listen,
        replicas: vec![r0],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        // Routing/affinity integration runs over plain h2c (no TLS material).
        server_tls: None,
        backend_tls: None,
    };
    tokio::spawn(server::run(cfg));
    tokio::time::sleep(Duration::from_millis(150)).await;
    let proxy = listen.to_string();

    let (status, _, grpc) = call(&proxy, UPDATE, Some("ghost-id")).await;
    assert_eq!(status, 200);
    assert_eq!(grpc.as_deref(), Some("9"));
    assert_eq!(
        hits.for_replica(0),
        0,
        "unknown conn_id must not reach upstream"
    );
}

/// Trace-context propagation (tier 1, always on): an incoming
/// `traceparent` / `tracestate` request header must reach the upstream
/// replica **unchanged**, so the gateway→storage trace is not severed at
/// the proxy hop. The fake replica echoes whatever trace headers it saw
/// back into its response so the client can assert byte-for-byte equality.
#[tokio::test]
async fn traceparent_is_forwarded_to_upstream_unchanged() {
    // A replica that mirrors the incoming traceparent/tracestate into its
    // response headers (prefixed so they survive as response metadata).
    async fn start_echo_replica() -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let (stream, _) = listener.accept().await.unwrap();
                let io = TokioIo::new(stream);
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<Incoming>| async move {
                        let tp = req
                            .headers()
                            .get("traceparent")
                            .and_then(|v| v.to_str().ok())
                            .unwrap_or("")
                            .to_string();
                        let ts = req
                            .headers()
                            .get("tracestate")
                            .and_then(|v| v.to_str().ok())
                            .unwrap_or("")
                            .to_string();
                        let body = Empty::<Bytes>::new()
                            .map_err(
                                |e: Infallible| -> Box<dyn std::error::Error + Send + Sync> {
                                    match e {}
                                },
                            )
                            .boxed();
                        let resp = Response::builder()
                            .status(200)
                            .header("content-type", "application/grpc")
                            .header("grpc-status", "0")
                            .header("x-seen-traceparent", tp)
                            .header("x-seen-tracestate", ts)
                            .body(body)
                            .unwrap();
                        Ok::<_, Infallible>(resp)
                    });
                    let _ = ConnBuilder::new(TokioExecutor::new())
                        .serve_connection(io, svc)
                        .await;
                });
            }
        });
        format!("{}:{}", addr.ip(), addr.port())
    }

    let replica = start_echo_replica().await;
    let listen = free_addr().await;
    let cfg = Config {
        listen,
        replicas: vec![replica],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        // Routing/affinity integration runs over plain h2c (no TLS material).
        server_tls: None,
        backend_tls: None,
    };
    tokio::spawn(server::run(cfg));
    tokio::time::sleep(Duration::from_millis(150)).await;
    let proxy = listen.to_string();

    const TRACEPARENT: &str = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
    const TRACESTATE: &str = "vendor1=val1,vendor2=val2";

    let client: hyper_util::client::legacy::Client<_, Empty<Bytes>> =
        hyper_util::client::legacy::Client::builder(TokioExecutor::new())
            .http2_only(true)
            .build(hyper_util::client::legacy::connect::HttpConnector::new());
    let req = Request::builder()
        .uri(format!("http://{proxy}/proj.notes.NotesQuery/Get"))
        .method("POST")
        .header("content-type", "application/grpc")
        .header("traceparent", TRACEPARENT)
        .header("tracestate", TRACESTATE)
        .body(Empty::<Bytes>::new())
        .unwrap();
    let resp = client.request(req).await.unwrap();

    let seen_tp = resp
        .headers()
        .get("x-seen-traceparent")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    let seen_ts = resp
        .headers()
        .get("x-seen-tracestate")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    let _ = resp.into_body().collect().await;

    assert_eq!(
        seen_tp, TRACEPARENT,
        "proxy must forward traceparent to upstream unchanged"
    );
    assert_eq!(
        seen_ts, TRACESTATE,
        "proxy must forward tracestate to upstream unchanged"
    );
}
