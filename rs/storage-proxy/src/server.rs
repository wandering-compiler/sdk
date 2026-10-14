//! Server bootstrap: build telemetry, bind, build shared routing state,
//! accept h2 connections, and drive a periodic affinity-map reaper.

use std::future::Future;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use std::sync::Once;
use std::time::Duration;

use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto::Builder as ConnBuilder;
use tokio::net::TcpListener;
use tokio_rustls::TlsAcceptor;

use crate::affinity::AffinityMap;
use crate::config::Config;
use crate::forward;
use crate::lb::RoundRobin;
use crate::otel::{self, Telemetry as OtelProviders};
use crate::router::{Router, UuidMinter};

/// Run the proxy until `shutdown` resolves (or a fatal bind error). The
/// accept loop runs until the shutdown future fires; on exit the
/// telemetry providers are flushed + shut down so the last metrics/spans
/// export. The provided `shutdown` future is the process's SIGINT signal.
pub async fn run_until<S>(
    cfg: Config,
    shutdown: S,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>>
where
    S: Future<Output = ()>,
{
    install_panic_logging();

    let affinity = Arc::new(AffinityMap::new(cfg.affinity_ttl));

    // Telemetry: gated entirely by config — when no OTLP endpoint is set,
    // this builds nothing and the forward-path handles are all `None`.
    let affinity_for_gauge = affinity.clone();
    let providers =
        OtelProviders::from_config(&cfg, Arc::new(move || affinity_for_gauge.len() as u64));
    let fwd_telemetry = forward::Telemetry {
        metrics: providers.metrics(),
        tracer: providers.tracer(),
    };

    let result = accept_loop(cfg, affinity, fwd_telemetry, shutdown).await;

    // Flush + shut down so the last metrics/spans are exported.
    providers.shutdown();
    result
}

/// Backward-compatible entry point (no external shutdown): runs forever.
/// Used by the integration harness which drops the spawned task.
pub async fn run(cfg: Config) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    run_until(cfg, std::future::pending::<()>()).await
}

async fn accept_loop<S>(
    cfg: Config,
    affinity: Arc<AffinityMap>,
    fwd_telemetry: forward::Telemetry,
    shutdown: S,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>>
where
    S: Future<Output = ()>,
{
    let router = Arc::new(Router {
        affinity: affinity.clone(),
        lb: Arc::new(RoundRobin::new(cfg.replicas.len())),
        minter: Arc::new(UuidMinter),
        replicas: cfg.replicas.clone(),
    });
    // Client leg: dial backends over TLS (`https`) when the stack-wide
    // W17_INTERNAL_TLS switch is on, else plain h2c (`http`) — the default.
    // The scheme is fixed for the process lifetime and threaded into every
    // forward.
    let backend_scheme = cfg.backend_scheme();
    let client = forward::build_client(cfg.backend_tls.as_ref()).map_err(to_boxed)?;

    // Server leg: terminate client TLS when the stack-wide W17_INTERNAL_TLS
    // switch is on; `None` ⇒ plain h2c listener (the default).
    let tls_acceptor: Option<TlsAcceptor> = match &cfg.server_tls {
        Some(t) => {
            let server_config = crate::tls::build_server_config(t).map_err(to_boxed)?;
            Some(TlsAcceptor::from(Arc::new(server_config)))
        }
        None => None,
    };

    spawn_reaper(affinity, cfg.affinity_ttl);

    let listener = TcpListener::bind(cfg.listen).await?;
    tracing::info!(
        listen = %cfg.listen,
        tls = tls_acceptor.is_some(),
        backend_scheme,
        "storage-proxy accepting h2"
    );

    tokio::pin!(shutdown);
    loop {
        let (stream, peer) = tokio::select! {
            _ = &mut shutdown => {
                tracing::info!("storage-proxy accept loop stopping");
                return Ok(());
            }
            accepted = listener.accept() => match accepted {
                Ok(v) => v,
                Err(e) => {
                    tracing::warn!(error = %e, "accept failed");
                    continue;
                }
            },
        };
        let router = router.clone();
        let client = client.clone();
        let telemetry = fwd_telemetry.clone();
        let tls_acceptor = tls_acceptor.clone();
        tokio::spawn(async move {
            // TLS handshake (when enabled) happens in the per-connection
            // task so a slow/failed handshake never stalls the accept loop.
            match tls_acceptor {
                Some(acceptor) => match acceptor.accept(stream).await {
                    Ok(tls) => {
                        serve_conn(
                            TokioIo::new(tls),
                            router,
                            client,
                            telemetry,
                            backend_scheme,
                            peer,
                        )
                        .await
                    }
                    Err(e) => tracing::debug!(%peer, error = %e, "tls handshake failed"),
                },
                None => {
                    serve_conn(
                        TokioIo::new(stream),
                        router,
                        client,
                        telemetry,
                        backend_scheme,
                        peer,
                    )
                    .await
                }
            }
        });
    }
}

/// Box a `String` config/setup error into the trait object the run loop
/// returns, so a bad TLS posture fails fast at bootstrap.
fn to_boxed(e: String) -> Box<dyn std::error::Error + Send + Sync> {
    e.into()
}

/// Serve one already-established (plaintext or TLS-terminated) h2
/// connection. Generic over the IO so the same body drives both legs.
async fn serve_conn<I>(
    io: I,
    router: Arc<Router>,
    client: forward::H2Client,
    telemetry: forward::Telemetry,
    backend_scheme: &'static str,
    peer: std::net::SocketAddr,
) where
    I: hyper::rt::Read + hyper::rt::Write + Unpin + Send + 'static,
{
    let svc = service_fn(move |req| {
        forward::handle(
            req,
            router.clone(),
            client.clone(),
            telemetry.clone(),
            backend_scheme,
        )
    });
    if let Err(e) = ConnBuilder::new(TokioExecutor::new())
        .serve_connection(io, svc)
        .await
    {
        tracing::debug!(%peer, error = %e, "connection ended");
    }
}

/// Install the W3C trace-context propagator. Called once at startup so the
/// per-RPC span path (when enabled) can extract incoming `traceparent`.
/// Trace-context *forwarding* itself is unconditional (headers are copied
/// verbatim in [`forward`]); this only affects the proxy's own spans.
pub fn install_propagator() {
    otel::install_propagator();
}

/// Periodically drop expired affinity entries so the map size stays
/// honest even for transactions whose caller vanished (the reaper, not a
/// lookup, then reclaims the entry). Cadence = TTL, capped at 60s.
fn spawn_reaper(affinity: Arc<AffinityMap>, ttl: Duration) {
    let period = ttl.min(Duration::from_secs(60)).max(Duration::from_secs(1));
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(period);
        loop {
            tick.tick().await;
            // reap_expired is synchronous (poison-tolerant lock + HashMap
            // ops — no reachable panic trigger today). Guard it anyway:
            // an unrecovered panic here would silently END this detached
            // task, so the affinity map would stop being GC'd for the
            // process lifetime. catch_unwind keeps the reaper alive to the
            // next tick; the panic hook already logs the cause.
            match std::panic::catch_unwind(AssertUnwindSafe(|| affinity.reap_expired())) {
                Ok(dropped) if dropped > 0 => {
                    tracing::debug!(
                        dropped,
                        size = affinity.len(),
                        "reaped expired affinity entries"
                    );
                }
                Ok(_) => {}
                Err(_) => tracing::error!("panic in affinity reaper tick; continuing"),
            }
        }
    });
}

/// Route panics in detached tasks (per-connection handlers, the reaper)
/// through `tracing` so they are observable in the proxy's logs, while
/// preserving the default hook's stderr backtrace. Tokio isolates a
/// panicking task (the process survives, only that task ends), so without
/// this the per-connection panic path would be invisible to structured
/// logging. Installed once at startup.
fn install_panic_logging() {
    static HOOK: Once = Once::new();
    HOOK.call_once(|| {
        let default = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            let loc = info
                .location()
                .map(|l| format!("{}:{}", l.file(), l.line()))
                .unwrap_or_else(|| "?".to_string());
            let msg = info
                .payload()
                .downcast_ref::<&str>()
                .copied()
                .or_else(|| info.payload().downcast_ref::<String>().map(String::as_str))
                .unwrap_or("<non-string panic payload>");
            tracing::error!(location = %loc, panic = %msg, "detached task panicked");
            default(info);
        }));
    });
}
