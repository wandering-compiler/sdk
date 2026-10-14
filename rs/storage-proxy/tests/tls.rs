//! End-to-end TLS round-trip: client →(TLS)→ proxy →(TLS)→ backend replica.
//!
//! Proves the re-encrypting hop actually works on the wire: the proxy
//! terminates the client's TLS (server leg), reads the routing headers,
//! then re-dials the chosen replica over its OWN TLS connection (client
//! leg, verifying the replica leaf against its pinned CA). A plaintext
//! client or replica could not complete this — the assertion is the
//! handshake itself succeeding end-to-end, not just the routing decision.

use std::convert::Infallible;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use http::Request;
use http_body_util::{BodyExt, Empty, Full};
use hyper::body::{Bytes, Incoming};
use hyper::service::service_fn;
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto::Builder as ConnBuilder;
use tokio::net::TcpListener;
use tokio_rustls::TlsAcceptor;

use storage_proxy::config::{BackendTls, Config, ServerTls};
use storage_proxy::{server, tls};

/// Mint a self-signed leaf for localhost/127.0.0.1, write cert+key to a
/// fresh temp dir, and return `(cert_path, key_path)`.
fn mint_cert(tag: &str) -> (String, String) {
    let ck =
        rcgen::generate_simple_self_signed(vec!["localhost".to_string(), "127.0.0.1".to_string()])
            .expect("mint self-signed leaf");
    let dir = std::env::temp_dir().join(format!("w17proxy-tls-{}-{}", std::process::id(), tag));
    std::fs::create_dir_all(&dir).unwrap();
    let cert_path = dir.join("server.crt");
    let key_path = dir.join("server.key");
    std::fs::write(&cert_path, ck.cert.pem()).unwrap();
    std::fs::write(&key_path, ck.key_pair.serialize_pem()).unwrap();
    (
        cert_path.to_string_lossy().into_owned(),
        key_path.to_string_lossy().into_owned(),
    )
}

/// A TLS-serving h2 fake replica: accepts TLS, replies `grpc-status: 0`,
/// counts how many requests it served. Returns its `host:port`.
async fn start_tls_replica(cert: &str, key: &str, hits: Arc<AtomicUsize>) -> String {
    let server_config = tls::build_server_config(&ServerTls {
        cert_path: cert.to_string(),
        key_path: key.to_string(),
        client_ca_path: None,
    })
    .expect("replica server config");
    let acceptor = TlsAcceptor::from(Arc::new(server_config));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let acceptor = acceptor.clone();
            let hits = hits.clone();
            tokio::spawn(async move {
                let tls = match acceptor.accept(stream).await {
                    Ok(t) => t,
                    Err(_) => return,
                };
                let io = TokioIo::new(tls);
                let svc = service_fn(move |_req: Request<Incoming>| {
                    let hits = hits.clone();
                    async move {
                        hits.fetch_add(1, Ordering::SeqCst);
                        let body = Full::new(Bytes::from_static(b"ok"))
                            .map_err(
                                |e: Infallible| -> Box<dyn std::error::Error + Send + Sync> {
                                    match e {}
                                },
                            )
                            .boxed();
                        Ok::<_, Infallible>(
                            http::Response::builder()
                                .status(200)
                                .header("content-type", "application/grpc")
                                .header("grpc-status", "0")
                                .body(body)
                                .unwrap(),
                        )
                    }
                });
                let _ = ConnBuilder::new(TokioExecutor::new())
                    .serve_connection(io, svc)
                    .await;
            });
        }
    });
    format!("127.0.0.1:{}", addr.port())
}

async fn free_addr() -> std::net::SocketAddr {
    TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap()
}

#[tokio::test]
async fn tls_round_trip_client_to_proxy_to_backend() {
    let (cert, key) = mint_cert("rt");
    let hits = Arc::new(AtomicUsize::new(0));
    let replica = start_tls_replica(&cert, &key, hits.clone()).await;

    let listen = free_addr().await;
    let cfg = Config {
        listen,
        replicas: vec![replica],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: None,
        trace_sample_ratio: 0.0,
        // Server leg: terminate the client's TLS with the minted leaf.
        server_tls: Some(ServerTls {
            cert_path: cert.clone(),
            key_path: key.clone(),
            client_ca_path: None,
        }),
        // Client leg: re-dial the backend over TLS, verifying it against the
        // minted leaf pinned as its own CA (SANs cover 127.0.0.1).
        backend_tls: Some(BackendTls {
            ca_path: Some(cert.clone()),
            client_identity: None,
        }),
    };
    tokio::spawn(server::run(cfg));
    // give the proxy a moment to bind + load its TLS material
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Build a TLS h2 client → proxy, verifying the proxy's leaf against the
    // minted cert pinned as its own CA (SANs cover 127.0.0.1).
    let client_cfg = tls::build_client_config(&BackendTls {
        ca_path: Some(cert.clone()),
        client_identity: None,
    })
    .unwrap();
    let mut http = HttpConnector::new();
    http.enforce_http(false);
    let https = hyper_rustls::HttpsConnectorBuilder::new()
        .with_tls_config(client_cfg)
        .https_or_http()
        .enable_http2()
        .wrap_connector(http);
    let client: Client<_, Empty<Bytes>> = Client::builder(TokioExecutor::new())
        .http2_only(true)
        .build(https);

    let req = Request::builder()
        .uri(format!("https://{listen}/proj.notes.NotesQuery/Get"))
        .method("POST")
        .header("content-type", "application/grpc")
        .body(Empty::<Bytes>::new())
        .unwrap();
    let resp = client
        .request(req)
        .await
        .expect("TLS request through the proxy should complete");
    assert_eq!(resp.status().as_u16(), 200);
    assert_eq!(
        resp.headers()
            .get("grpc-status")
            .and_then(|v| v.to_str().ok()),
        Some("0"),
        "proxy forwarded the backend's gRPC OK over TLS"
    );
    let _ = resp.into_body().collect().await;
    assert_eq!(
        hits.load(Ordering::SeqCst),
        1,
        "the backend replica must have been reached over the proxy's TLS client leg"
    );
}
