//! HTTP/2 transparent forwarding. Bodies are streamed opaquely in both
//! directions — the proxy never buffers or decodes a protobuf message.
//!
//! The routing *decision* lives in [`crate::router::decide`]; this module
//! only carries out the I/O the decision dictates (open an h2 connection
//! to the chosen replica, splice the body streams, and apply the
//! conn_id stamp / eviction side effects).

use std::convert::Infallible;
use std::sync::Arc;
use std::time::Instant;

use http::{HeaderName, HeaderValue, Request, Response, StatusCode};
use http_body_util::combinators::BoxBody;
use http_body_util::{BodyExt, Empty};
use hyper::body::Incoming;
use hyper_rustls::HttpsConnector;
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::client::legacy::Client;
use hyper_util::rt::TokioExecutor;

use crate::config::BackendTls;
use crate::otel::{self, Metrics, MetricsExt, ProxyTracer};
use crate::router::{Route, Router, CONN_ID_HEADER};

/// gRPC status codes used on the proxy's own outcomes.
const GRPC_OK: i32 = 0;
const GRPC_FAILED_PRECONDITION: i32 = 9;
const GRPC_UNAVAILABLE: i32 = 14;

type ResBody = BoxBody<bytes::Bytes, Box<dyn std::error::Error + Send + Sync>>;

/// The shared upstream h2 client. Always an HTTPS-capable connector so the
/// type is uniform; the actual scheme dialed (`http`/`https`) is chosen
/// per-request from [`crate::config::Config::backend_scheme`]. When the
/// client leg is plaintext the rustls config is a never-exercised
/// placeholder (only `http://` URIs flow).
pub type H2Client = Client<HttpsConnector<HttpConnector>, Incoming>;

/// Build the shared upstream h2 client. `backend_tls = Some` ⇒ dial
/// backends over TLS (verified against a pinned CA or the system roots);
/// `None` ⇒ plaintext h2c (`http://`).
pub fn build_client(backend_tls: Option<&BackendTls>) -> Result<H2Client, String> {
    let mut http = HttpConnector::new();
    http.set_nodelay(true);
    // Let https:// URIs through the inner connector (it defaults to
    // http-only); the HttpsConnector wrapper picks TLS vs plain by scheme.
    http.enforce_http(false);

    let tls_config = match backend_tls {
        Some(t) => crate::tls::build_client_config(t)?,
        None => crate::tls::placeholder_client_config()?,
    };

    let https = hyper_rustls::HttpsConnectorBuilder::new()
        .with_tls_config(tls_config)
        .https_or_http()
        .enable_http2()
        .wrap_connector(http);

    Ok(Client::builder(TokioExecutor::new())
        .http2_only(true)
        .build(https))
}

/// Cheap, clonable bundle of telemetry handles threaded into the forward
/// path. Both fields are `None` when disabled (zero per-RPC overhead).
#[derive(Clone, Default)]
pub struct Telemetry {
    /// Metric instruments, or `None` when no OTLP endpoint is configured.
    pub metrics: Option<Metrics>,
    /// Sampling tracer, or `None` when per-RPC spans are off (default).
    pub tracer: Option<ProxyTracer>,
}

/// Handle one incoming gRPC request: decide, forward (or reject), apply
/// conn_id side effects, and record telemetry. Never reads the
/// request/response body. Telemetry is a no-op when disabled.
pub async fn handle(
    req: Request<Incoming>,
    router: Arc<Router>,
    client: H2Client,
    telemetry: Telemetry,
    backend_scheme: &'static str,
) -> Result<Response<ResBody>, Infallible> {
    let path = req.uri().path().to_string();
    let conn_id = req
        .headers()
        .get(CONN_ID_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string());

    let route = crate::router::decide(
        &path,
        conn_id.as_deref(),
        &router.affinity,
        &router.lb,
        router.minter.as_ref(),
    );

    // Per-RPC span: only constructed when the tracer is present (opt-in +
    // sampled). Parented to the incoming traceparent so the proxy hop
    // joins the gateway→storage trace. When `tracer` is None this whole
    // block is skipped — no span machinery on the hot path.
    let mut span = telemetry.tracer.as_ref().map(|t| {
        let parent = otel::extract_parent(req.headers());
        t.start(route.class(), &parent)
    });

    match route {
        Route::Reject {
            grpc_status,
            message,
            class,
        } => {
            tracing::debug!(%path, ?class, grpc_status, "reject");
            telemetry.metrics.routed(class);
            telemetry.metrics.route_error(reject_reason(grpc_status));
            if let Some(s) = span.take() {
                s.finish(grpc_status);
            }
            Ok(grpc_error_response(grpc_status, &message))
        }
        Route::Forward {
            replica,
            class,
            stamp_conn_id,
            evict,
        } => {
            let authority = router.authority(replica).to_string();
            tracing::debug!(%path, ?class, replica, %authority, "forward");
            telemetry.metrics.routed(class);
            if let Some(s) = span.as_mut() {
                s.set_replica(replica);
            }
            let started = Instant::now();
            match forward(req, &authority, client, backend_scheme).await {
                Ok(resp) => {
                    telemetry
                        .metrics
                        .forward_duration(class, elapsed_ms(started));
                    let resp = apply_stamp(resp, stamp_conn_id);
                    if let Some(id) = evict {
                        router.affinity.evict(&id);
                    }
                    if let Some(s) = span.take() {
                        s.finish(GRPC_OK);
                    }
                    Ok(resp)
                }
                Err(e) => {
                    telemetry
                        .metrics
                        .forward_duration(class, elapsed_ms(started));
                    telemetry.metrics.route_error("upstream_unavailable");
                    tracing::warn!(%authority, error = %e, "upstream forward failed");
                    if let Some(s) = span.take() {
                        s.finish(GRPC_UNAVAILABLE);
                    }
                    Ok(grpc_error_response(
                        GRPC_UNAVAILABLE,
                        &format!("storage replica {authority} unavailable: {e}"),
                    ))
                }
            }
        }
    }
}

fn elapsed_ms(since: Instant) -> f64 {
    since.elapsed().as_secs_f64() * 1000.0
}

/// Map a reject's gRPC status to a low-cardinality `reason` label.
fn reject_reason(grpc_status: i32) -> &'static str {
    match grpc_status {
        GRPC_FAILED_PRECONDITION => "unknown_conn",
        GRPC_UNAVAILABLE => "upstream_unavailable",
        _ => "unknown_conn",
    }
}

/// Splice the request to `authority` and return the upstream response
/// with its body stream intact. All headers are forwarded verbatim
/// (including W3C `traceparent`/`tracestate`).
async fn forward(
    req: Request<Incoming>,
    authority: &str,
    client: H2Client,
    scheme: &str,
) -> Result<Response<ResBody>, Box<dyn std::error::Error + Send + Sync>> {
    let (mut parts, body) = req.into_parts();

    // Re-point the URI at the chosen replica; keep path+query verbatim.
    let pq = parts
        .uri
        .path_and_query()
        .map(|p| p.as_str())
        .unwrap_or("/")
        .to_string();
    let uri = format!("{scheme}://{authority}{pq}");
    // `pq` is copied verbatim from the *incoming* request — the one
    // place externally-influenced bytes are re-parsed. h2 normally
    // guarantees a valid `:path`, but a bad-URI edge case must surface
    // as a clean gRPC error (the caller maps Err → GRPC_UNAVAILABLE),
    // never a connection-killing panic.
    parts.uri = uri
        .parse()
        .map_err(|e| format!("rebuild upstream uri {uri:?}: {e}"))?;

    let upstream_req = Request::from_parts(parts, body);
    let resp = client.request(upstream_req).await?;

    let (resp_parts, resp_body) = resp.into_parts();
    let boxed = resp_body
        .map_err(|e| Box::new(e) as Box<dyn std::error::Error + Send + Sync>)
        .boxed();
    Ok(Response::from_parts(resp_parts, boxed))
}

/// Inject `w17-conn-id` into the response headers without touching the
/// body (the Begin stamp). gRPC clients read response *metadata*, which
/// hyper/h2 surfaces as the leading response headers.
fn apply_stamp(mut resp: Response<ResBody>, stamp: Option<String>) -> Response<ResBody> {
    if let Some(id) = stamp {
        if let Ok(val) = HeaderValue::from_str(&id) {
            resp.headers_mut()
                .insert(HeaderName::from_static(CONN_ID_HEADER), val);
        }
    }
    resp
}

/// Build a body-less gRPC error: HTTP 200 with `grpc-status` /
/// `grpc-message` carried as **leading** headers (a "trailers-only"
/// response), which is a valid gRPC error shape that needs no body.
fn grpc_error_response(status: i32, message: &str) -> Response<ResBody> {
    let body = Empty::<bytes::Bytes>::new()
        .map_err(|never| match never {})
        .boxed();
    let mut resp = Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "application/grpc")
        .header("grpc-status", status.to_string())
        .body(body)
        .expect("static grpc error response is valid");
    if let Ok(val) = HeaderValue::from_str(message) {
        resp.headers_mut()
            .insert(HeaderName::from_static("grpc-message"), val);
    }
    resp
}
