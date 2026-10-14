//! storage-proxy — a contract-blind L7 gRPC proxy.
//!
//! Routes gRPC (HTTP/2) requests to one of N identical storage replicas,
//! pinning every RPC of a distributed transaction to the replica that
//! opened it. It inspects only the HTTP/2 `:path` pseudo-header and the
//! `w17-conn-id` gRPC metadata header — it NEVER decodes protobuf
//! message bodies, so one build serves every project and never recompiles
//! when a schema changes.

use storage_proxy::config::Config;
use storage_proxy::server;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let cfg = match Config::from_env() {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("configuration error: {e}");
            std::process::exit(2);
        }
    };

    tracing::info!(
        listen = %cfg.listen,
        replicas = ?cfg.replicas,
        affinity_ttl_secs = cfg.affinity_ttl.as_secs(),
        otel_endpoint = ?cfg.otel_endpoint,
        trace_sample_ratio = cfg.trace_sample_ratio,
        "storage-proxy starting (body-blind: message bodies are never decoded)"
    );

    // Trace context propagation is unconditional (forwarded verbatim);
    // installing the propagator only lets the proxy's own opt-in spans
    // parent onto the incoming traceparent.
    server::install_propagator();

    let shutdown = async {
        let _ = tokio::signal::ctrl_c().await;
        tracing::info!("shutdown signal received");
    };

    // `run_until` owns the telemetry lifecycle: it builds the providers,
    // runs the accept loop until `shutdown` fires, then flushes + shuts
    // them down so the last metrics/spans are exported.
    if let Err(e) = server::run_until(cfg, shutdown).await {
        tracing::error!("server error: {e}");
        std::process::exit(1);
    }
}
