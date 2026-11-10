//! The business tier's lifecycle: boot from the manifest, serve, drain.
//!
//! `w17_business::main!(register_business)` hands the author's register
//! function to [`run`], which does what Go's generated business `main` does:
//!
//! 1. parse `serve` / `health`, `--manifest` (default
//!    `$W17_BUSINESS_MANIFEST`, then `/app/w17-business.json`);
//! 2. read the manifest, refuse a `language` other than `rust`, and refuse a
//!    descriptor set other than the one the binary was compiled from;
//! 3. the config (a value that does not parse, or a required key with no
//!    value, stops the boot naming it), Sentry and OpenTelemetry;
//! 4. the buses: NATS and/or Redis Streams, each DSN resolved through the
//!    secrets chain; a bus whose DSN is empty is off and its events are
//!    dropped, as Go leaves the dispatcher nil;
//! 5. the storage channel and, with a `companion` section, the companion's
//!    (both dialled lazily);
//! 6. `register_business(cfg, registry, clients)`; every method a plugin hook
//!    is bound to must then have a handler;
//! 7. serve h2c, or TLS with `W17_INTERNAL_TLS`; `grpc.health.v1`;
//!    reflection when `<PREFIX>_GRPC_REFLECTION` is `1`/`true`;
//! 8. on SIGTERM/SIGINT, ONE budget for the whole drain — `max(30 s,
//!    shutdown_timeout_seconds)` from the start: NOT_SERVING, stop accepting,
//!    give the in-flight calls `min(timeout, left)`, then CANCEL the ones
//!    still running (their futures are dropped); the work committed
//!    transactions still owe (events, `on_commit`) gets the rest of the
//!    window, then the shutdown hooks, the bus close and the channels, each
//!    bounded by what is left; the telemetry flush (at most 5 s) after.

use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    process::ExitCode,
    sync::{Arc, atomic::Ordering},
    time::{Duration, Instant},
};

use hyper_util::{
    rt::{TokioExecutor, TokioIo, TokioTimer},
    server::graceful::GracefulShutdown,
};
use prost_reflect::DescriptorPool;
use sha2::{Digest as _, Sha256};
use tokio::net::TcpListener;
use tonic::transport::Endpoint;

use crate::{
    BootError,
    channel::{CompanionChannel, KEEPALIVE_TIME, KEEPALIVE_TIMEOUT, Outbound, StorageChannel},
    config::Config,
    ctx::Ctx,
    distx::Coordinator,
    events::{Dispatcher, NatsPublisher, RedisPublisher, Router},
    files::FileStore,
    manifest::{self, Manifest},
    observability, principal,
    secrets::{self, Env},
    server::{Registry, Served},
    tlsconfig, txscope,
};

/// lifecycle.DefaultShutdownTimeout; the manifest's
/// shutdown_timeout_seconds wins.
pub const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(10);
/// runtime.shutdownDrainTimeout: the budget of the whole drain, unless the
/// shutdown timeout is longer.
pub const DRAIN_TIMEOUT: Duration = Duration::from_secs(30);
pub const ENV_MANIFEST: &str = "W17_BUSINESS_MANIFEST";
pub const DEFAULT_MANIFEST: &str = "/app/w17-business.json";
pub const DEFAULT_HEALTH_TIMEOUT: Duration = Duration::from_secs(3);
/// grpc-go's ConnectionTimeout: a connection that has not finished its TLS
/// handshake by then is closed.
const TLS_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(120);
/// The pause after a failed accept.
const ACCEPT_BACKOFF: Duration = Duration::from_millis(100);

/// The author's register function, as [`crate::main!`] takes it.
pub type RegisterFn = fn(&Config, &mut Registry, &Clients) -> Result<(), BootError>;

/// `host:port` / `[v6]:port` / `:port` (localhost).
pub fn parse_addr(addr: &str) -> Result<(String, u16), String> {
    let (host, port) = if let Some(rest) = addr.strip_prefix('[') {
        match rest.split_once(']') {
            Some((h, p)) => (h.to_string(), p.trim_start_matches(':').to_string()),
            None => (rest.to_string(), String::new()),
        }
    } else {
        match addr.rsplit_once(':') {
            Some((h, p)) => (h.to_string(), p.to_string()),
            None => (String::new(), addr.to_string()),
        }
    };
    if port.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) {
        return Err(format!("address {addr:?} has no port"));
    }
    let port: u16 = port
        .parse()
        .map_err(|_| format!("address {addr:?} has no port"))?;
    Ok((
        if host.is_empty() {
            "localhost".into()
        } else {
            host
        },
        port,
    ))
}

fn target(host: &str, port: u16) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// The tier's clients, built from the manifest's `storage` and `companion`
/// sections.
///
/// [`Clients::storage`] is the channel every storage service is reached
/// through (`OrderMutationClient::new(clients.storage())`),
/// [`Clients::distributed_transaction`] the coordinator [`crate::distx`]
/// drives, [`Clients::egress`] an egress client's channel
/// (`QuotesApiClient::new(clients.egress("quotes")?)`) and [`Clients::store`]
/// an upload connection's [`FileStore`].
pub struct Clients {
    storage: StorageChannel,
    tx: Option<Coordinator>,
    egress: Vec<(String, String, CompanionChannel)>,
    stores: HashMap<String, FileStore>,
}

fn channel(addr: &str, env: &Env, what: &str) -> Result<tonic::transport::Channel, BootError> {
    let (host, port) = parse_addr(addr).map_err(|e| BootError(format!("{what}: {e}")))?;
    let tls = tlsconfig::client_tls(env).map_err(|e| BootError(format!("{what} tls: {e}")))?;
    let scheme = if tls.is_some() { "https" } else { "http" };
    let mut ep = Endpoint::from_shared(format!("{scheme}://{}", target(&host, port)))
        .map_err(|e| BootError(format!("{what}: {addr:?}: {e}")))?
        .http2_keep_alive_interval(KEEPALIVE_TIME)
        .keep_alive_timeout(KEEPALIVE_TIMEOUT)
        .keep_alive_while_idle(true);
    if let Some(t) = tls {
        ep = ep
            .tls_config(t)
            .map_err(|e| BootError(format!("{what} tls: {e}")))?;
    }
    Ok(ep.connect_lazy())
}

impl Clients {
    pub(crate) fn new(pool: &DescriptorPool, m: &Manifest, env: &Env) -> Result<Self, BootError> {
        let mut streaming: HashSet<String> = pool
            .services()
            .flat_map(|s| {
                s.methods()
                    .filter(|m| m.is_client_streaming())
                    .map(|m| format!("/{}/{}", m.parent_service().full_name(), m.name()))
                    .collect::<Vec<_>>()
            })
            .collect();
        // The companion's Put, whether or not the descriptor set carries files.proto.
        streaming.insert(format!("/{}/Put", crate::files::SERVICE));
        let (storage, tx) = match &m.storage {
            None => (
                StorageChannel(Arc::new(Outbound::refusing(format!(
                    "clients: the {} domain has no storage tier",
                    m.domain
                )))),
                None,
            ),
            Some(tier) => {
                let addr = env
                    .get(&tier.address_env)
                    .filter(|a| !a.is_empty())
                    .cloned()
                    .unwrap_or(tier.default_address.clone());
                if addr.is_empty() {
                    return Err(BootError(format!(
                        "storage: {} is not set and the manifest gives no default address",
                        tier.address_env
                    )));
                }
                parse_addr(&addr).map_err(|e| {
                    BootError(format!("storage: {}={addr:?}: {e}", tier.address_env))
                })?;
                for svc in &tier.services {
                    if pool
                        .get_service_by_name(svc.trim_start_matches('.'))
                        .is_none()
                    {
                        return Err(BootError(format!(
                            "storage: service {svc:?} is not in the descriptor set"
                        )));
                    }
                }
                let ch = StorageChannel(Arc::new(Outbound::new(
                    channel(&addr, env, "storage")?,
                    false,
                    streaming.clone(),
                )));
                let tx = (!tier.distributed_transactions.is_empty())
                    .then(|| Coordinator::new(ch.clone()));
                (ch, tx)
            }
        };
        let mut out = Clients {
            storage,
            tx,
            egress: Vec::new(),
            stores: HashMap::new(),
        };
        let Some(comp) = &m.companion else {
            return Ok(out);
        };
        let addr = env
            .get(&comp.address_env)
            .filter(|a| !a.is_empty())
            .cloned()
            .unwrap_or(comp.default_address.clone());
        if addr.is_empty() {
            return Err(BootError(format!(
                "companion: {} is not set and the manifest gives no default address",
                comp.address_env
            )));
        }
        parse_addr(&addr)
            .map_err(|e| BootError(format!("companion: {}={addr:?}: {e}", comp.address_env)))?;
        let ch = CompanionChannel(Arc::new(Outbound::new(
            channel(&addr, env, "companion")?,
            true,
            streaming,
        )));
        for e in &comp.egress {
            if pool
                .get_service_by_name(e.service.trim_start_matches('.'))
                .is_none()
            {
                return Err(BootError(format!(
                    "companion: egress client {}: service {:?} is not in the descriptor set",
                    e.client, e.service
                )));
            }
            out.egress
                .push((e.client.clone(), e.service.clone(), ch.clone()));
        }
        if let Some(files) = &comp.files {
            // The manifest reader holds the service to W17Files, which the SDK
            // compiles itself: the descriptor set need not carry it.
            let placements = Arc::new(files.placements.clone());
            for c in &files.connections {
                out.stores.insert(
                    c.name.clone(),
                    FileStore::new(ch.clone(), c.clone(), placements.clone()),
                );
            }
        }
        Ok(out)
    }

    /// The channel to the storage tier, for every storage service's client.
    /// A domain with no storage tier gets a channel that refuses every call.
    pub fn storage(&self) -> StorageChannel {
        self.storage.clone()
    }

    /// The distributed-transaction coordinator; refused when this domain's
    /// storage registers none (it declares no mutation service).
    pub fn distributed_transaction(&self) -> Result<Coordinator, BootError> {
        self.tx.clone().ok_or_else(|| {
            BootError(
                "clients: this domain's storage registers no distributed-transaction coordinator (it declares no \
                 mutation service), so there is no transaction to begin"
                    .into(),
            )
        })
    }

    /// The channel of the egress client `name` (`"quotes"`) or of the one
    /// serving the service `name` (`"clients.quotes.QuotesApi"`).
    pub fn egress(&self, name: &str) -> Result<CompanionChannel, BootError> {
        let n = name.trim_start_matches('.');
        if let Some((_, _, ch)) = self.egress.iter().find(|(c, s, _)| c == n || s == n) {
            return Ok(ch.clone());
        }
        let mut have: Vec<&str> = self.egress.iter().map(|(c, _, _)| c.as_str()).collect();
        have.sort();
        Err(BootError(format!(
            "clients: no egress client {name:?} (have: {})",
            have.join(", ")
        )))
    }

    /// The file store of the upload connection `connection`.
    pub fn store(&self, connection: &str) -> Result<FileStore, BootError> {
        self.stores.get(connection).cloned().ok_or_else(|| {
            let mut have: Vec<&str> = self.stores.keys().map(String::as_str).collect();
            have.sort();
            BootError(format!(
                "clients: no upload connection {connection:?} (have: {})",
                have.join(", ")
            ))
        })
    }
}

/// The SHA-256 of a descriptor set, as the build helper computes it.
pub fn descriptors_digest(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

/// One business tier, booted from a manifest.
pub(crate) struct App {
    manifest: Manifest,
    env: Env,
    descriptors: &'static [u8],
    registry: Registry,
    dispatcher: Option<Arc<dyn Dispatcher>>,
    served: Option<Served>,
    health: Option<tonic_health::server::HealthReporter>,
    drain_timeout: Duration,
}

impl App {
    /// Steps 2–6 of the boot.
    pub(crate) async fn boot(
        manifest: Manifest,
        env: Env,
        digest: &str,
        register: RegisterFn,
    ) -> Result<Self, BootError> {
        if manifest.language != manifest::LANGUAGE {
            return Err(BootError(format!(
                "manifest: language {:?} is not rust",
                manifest.language
            )));
        }
        let set = manifest.path(&manifest.descriptor_set);
        let raw = std::fs::read(&set)
            .map_err(|e| BootError(format!("descriptor_set: {}: {e}", set.display())))?;
        let have = descriptors_digest(&raw);
        if !digest.is_empty() && have != digest {
            return Err(BootError(format!(
                "descriptor_set: {} (sha256 {have}) is not the descriptor set this binary was compiled from (sha256 \
                 {digest}) — rebuild the crate against this manifest's descriptors.binpb",
                set.display()
            )));
        }
        let descriptors: &'static [u8] = Box::leak(raw.into_boxed_slice());
        let pool = DescriptorPool::decode(descriptors)
            .map_err(|e| BootError(format!("descriptor_set: {}: {e}", set.display())))?;
        let auth: Vec<_> = pool
            .all_messages()
            .filter(|d| d.name() == "AuthResp")
            .collect();
        if auth.len() == 1 {
            principal::set_auth_resp_type(auth.into_iter().next());
        }
        let resolver = if manifest.config.iter().any(|k| k.secret) || manifest.events.is_some() {
            Some(secrets::new_resolver(&env).map_err(|e| BootError(format!("secrets: {e}")))?)
        } else {
            None
        };
        let cfg =
            Config::load(&manifest.config, &env, resolver.as_ref()).map_err(|e| BootError(e.0))?;
        observability::init(&manifest.bundle, cfg.secret_env_names(), &env)
            .map_err(|e| BootError(format!("observability: {e}")))?;
        let dispatcher = Self::bus(&manifest, resolver.as_ref()).await?;
        let clients = Clients::new(&pool, &manifest, &env)?;
        let services: Vec<String> = manifest.services.iter().map(|s| s.name.clone()).collect();
        let mut registry = Registry::new(
            pool.clone(),
            &services,
            manifest.emits(),
            manifest.invalidates(),
        );
        for e in manifest.emits() {
            if pool.get_message_by_name(&e.envelope).is_none()
                || pool.get_message_by_name(&e.event).is_none()
            {
                return Err(BootError(format!(
                    "events: {}: the envelope {} or the event {} is not in the descriptor set",
                    e.method, e.envelope, e.event
                )));
            }
        }
        register(&cfg, &mut registry, &clients)?;
        let served: HashSet<&str> = registry.paths().collect();
        for hook in &manifest.plugin_hooks {
            for path in &hook.methods {
                if !served.contains(path.as_str()) {
                    return Err(BootError(format!(
                        "plugin hook {}/{} ({}) calls {path}, which has no handler: register its service, or the \
                         plugin's call fails at runtime",
                        hook.plugin, hook.slot, hook.registered_as
                    )));
                }
            }
        }
        for svc in &manifest.services {
            if !registry.registered().contains(&svc.name) {
                observability::info(&format!(
                    "{} is not registered; its calls answer Unimplemented, as in Go",
                    svc.name
                ));
            }
        }
        Ok(App {
            manifest,
            env,
            descriptors,
            registry,
            dispatcher,
            served: None,
            health: None,
            drain_timeout: DRAIN_TIMEOUT,
        })
    }

    async fn bus(
        m: &Manifest,
        resolver: Option<&secrets::Resolver>,
    ) -> Result<Option<Arc<dyn Dispatcher>>, BootError> {
        let Some(ev) = &m.events else { return Ok(None) };
        if ev.nats_dsn_env.is_empty() && ev.redis_dsn_env.is_empty() {
            return Ok(None);
        }
        let get = |k: &str| {
            if k.is_empty() {
                String::new()
            } else {
                resolver.map(|r| r.get(k)).unwrap_or_default()
            }
        };
        let nats_dsn = get(&ev.nats_dsn_env);
        let nats: Option<Box<dyn Dispatcher>> = if nats_dsn.is_empty() {
            None
        } else {
            Some(Box::new(
                NatsPublisher::connect(&nats_dsn, Duration::from_secs(30))
                    .await
                    .map_err(|e| BootError(format!("eventbus nats: {}: {e}", ev.nats_dsn_env)))?
                    .with_domain(&m.domain),
            ))
        };
        let redis_dsn = get(&ev.redis_dsn_env);
        let redis: Option<Box<dyn Dispatcher>> = if redis_dsn.is_empty() {
            None
        } else {
            Some(Box::new(
                RedisPublisher::connect(&redis_dsn)
                    .await
                    .map_err(|e| BootError(format!("eventbus redis: {}: {e}", ev.redis_dsn_env)))?
                    .with_domain(&m.domain),
            ))
        };
        if nats.is_none() && redis.is_none() {
            return Ok(None);
        }
        Ok(Some(Arc::new(Router {
            nats,
            redis,
            redis_channels: ev.redis_channels.iter().cloned().collect(),
        })))
    }

    fn listen_addr(&self) -> String {
        self.env
            .get(&self.manifest.listen_env)
            .filter(|v| !v.is_empty())
            .cloned()
            .unwrap_or_else(|| self.manifest.default_listen_addr.clone())
    }

    /// Serve until `stop` resolves, then drain.
    pub(crate) async fn run(
        mut self,
        stop: impl std::future::Future<Output = ()>,
    ) -> Result<(), BootError> {
        let tls =
            tlsconfig::server_tls(&self.env).map_err(|e| BootError(format!("server tls: {e}")))?;
        let listen = self.listen_addr();
        let (host, port) = parse_addr(&listen)
            .map_err(|e| BootError(format!("{}={listen:?}: {e}", self.manifest.listen_env)))?;
        // ":50051" listens on every interface, as in Go.
        let host = if listen.starts_with(':') {
            "0.0.0.0".to_string()
        } else {
            host
        };
        let listener = TcpListener::bind(target(&host, port))
            .await
            .map_err(|e| BootError(format!("listen {listen}: {e}")))?;
        let (reporter, health) = tonic_health::server::health_reporter();
        reporter
            .set_service_status("", tonic_health::ServingStatus::NotServing)
            .await;
        let reflection = {
            let v = self
                .env
                .get(&self.manifest.reflection_env)
                .map(String::as_str)
                .unwrap_or("");
            v == "1" || v == "true"
        };
        let descriptors = self.descriptors;
        let served = self.registry.finish(self.dispatcher.clone(), |mut routes| {
            routes = routes.add_service(health);
            if reflection {
                let v1 = tonic_reflection::server::Builder::configure()
                    .register_encoded_file_descriptor_set(descriptors)
                    .register_encoded_file_descriptor_set(tonic_health::pb::FILE_DESCRIPTOR_SET)
                    .build_v1();
                let v1alpha = tonic_reflection::server::Builder::configure()
                    .register_encoded_file_descriptor_set(descriptors)
                    .register_encoded_file_descriptor_set(tonic_health::pb::FILE_DESCRIPTOR_SET)
                    .build_v1alpha();
                match (v1, v1alpha) {
                    (Ok(a), Ok(b)) => routes = routes.add_service(a).add_service(b),
                    (Err(e), _) | (_, Err(e)) => observability::warn(&format!("reflection: {e}")),
                }
            }
            routes
        });
        self.served = Some(served.clone());
        let local = listener
            .local_addr()
            .map_err(|e| BootError(format!("listen {listen}: {e}")))?;
        reporter
            .set_service_status("", tonic_health::ServingStatus::Serving)
            .await;
        self.health = Some(reporter);
        observability::info(&format!(
            "{} listening on {local}{}",
            self.manifest.bundle,
            if tls.is_some() { " (TLS)" } else { "" }
        ));
        let graceful = GracefulShutdown::new();
        let acceptor = tls.map(tokio_rustls::TlsAcceptor::from);
        tokio::pin!(stop);
        loop {
            tokio::select! {
                _ = &mut stop => break,
                accepted = listener.accept() => {
                    let stream = match accepted {
                        Ok((stream, _)) => stream,
                        Err(e) => {
                            // Out of descriptors, mostly: accept fails at once
                            // again until one is closed, so pause rather than
                            // spin (what Go's grpc server does).
                            observability::warn(&format!("accept: {e}"));
                            tokio::time::sleep(ACCEPT_BACKOFF).await;
                            continue;
                        }
                    };
                    let _ = stream.set_nodelay(true);
                    let served = served.clone();
                    let acceptor = acceptor.clone();
                    let watcher = graceful.watcher();
                    tokio::spawn(async move {
                        let svc = hyper::service::service_fn(move |req| {
                            let served = served.clone();
                            async move { Ok::<_, std::convert::Infallible>(served.serve_wire(req).await) }
                        });
                        let mut builder = hyper::server::conn::http2::Builder::new(TokioExecutor::new());
                        builder
                            .timer(TokioTimer::new())
                            .keep_alive_interval(KEEPALIVE_TIME)
                            .keep_alive_timeout(KEEPALIVE_TIMEOUT);
                        let result = match acceptor {
                            None => watcher.watch(builder.serve_connection(TokioIo::new(stream), svc)).await,
                            Some(a) => match tokio::time::timeout(TLS_HANDSHAKE_TIMEOUT, a.accept(stream)).await {
                                Ok(Ok(tls)) => watcher.watch(builder.serve_connection(TokioIo::new(tls), svc)).await,
                                _ => return,
                            },
                        };
                        drop(result);
                    });
                }
            }
        }
        drop(listener);
        self.shutdown(graceful).await;
        Ok(())
    }

    async fn shutdown(&mut self, graceful: GracefulShutdown) {
        let timeout = if self.manifest.shutdown_timeout.is_zero() {
            SHUTDOWN_TIMEOUT
        } else {
            self.manifest.shutdown_timeout
        };
        observability::info(&format!("{}: shutting down", self.manifest.bundle));
        // One budget for the whole drain, set once, now: every step below
        // draws from what is left of it, so a deployment's termination grace
        // bounds the drain as a whole, not each step alone.
        let budget = self.drain_timeout.max(timeout);
        let start = Instant::now();
        let left = || budget.saturating_sub(start.elapsed());
        if let Some(h) = &self.health {
            h.set_service_status("", tonic_health::ServingStatus::NotServing)
                .await;
        }
        let Some(served) = self.served.clone() else {
            return;
        };
        served.shared.draining.store(true, Ordering::SeqCst);
        // Go's GracefulStop bounded by the timeout, then Stop: GOAWAY on
        // every connection at once, so a client takes its next call to
        // another replica instead of being answered UNAVAILABLE here; the
        // in-flight calls finish, then whatever is left is cancelled.
        let closing = tokio::spawn(async move { graceful.shutdown().await });
        let stop = timeout.min(left());
        if tokio::time::timeout(stop, served.shared.wait_idle(stop))
            .await
            .map(|idle| !idle)
            .unwrap_or(true)
        {
            observability::report_error(&format!(
                "runtime: gRPC handler(s) still running after {}ms; cancelling them",
                stop.as_millis()
            ));
        }
        served.shared.kill.cancel();
        // After the server's stop, one more timeout (within the budget),
        // shared by the two waits: the handlers it cancelled (a detached
        // rollback, a commit) and the work a committed transaction owes.
        let after_stop = Instant::now() + timeout.min(left());
        let until = || after_stop.saturating_duration_since(Instant::now());
        if !served.shared.wait_idle(until()).await {
            observability::report_error(&format!(
                "runtime: {} gRPC handler(s) still running after the server closed; continuing without them",
                served.shared.inflight()
            ));
        }
        if !txscope::drain(until()).await {
            observability::report_error(&format!(
                "runtime: {} piece(s) of work after a commit (events, on_commit hooks) still running at the shutdown \
                 deadline; abandoned",
                txscope::pending_count()
            ));
        }
        let remaining = || left().max(Duration::from_millis(1));
        let drain_ctx = Ctx::background().with_timeout(left());
        for (i, hook) in self.registry.take_shutdowns().into_iter().enumerate() {
            if tokio::time::timeout(remaining(), hook(drain_ctx.clone()))
                .await
                .is_err()
            {
                observability::report_error(&format!("runtime: shutdown hook {i}: timed out"));
            }
        }
        if let Some(d) = &self.dispatcher
            && tokio::time::timeout(remaining(), d.close()).await.is_err()
        {
            observability::warn("eventbus close: timed out");
        }
        closing.abort();
        observability::shutdown(Duration::from_secs(5));
    }
}

/// The manifest path: the flag, else `$W17_BUSINESS_MANIFEST`, else
/// `/app/w17-business.json`.
pub fn resolve_manifest_path(arg: Option<&str>, env: &Env) -> PathBuf {
    arg.map(str::to_string)
        .or_else(|| env.get(ENV_MANIFEST).filter(|v| !v.is_empty()).cloned())
        .unwrap_or_else(|| DEFAULT_MANIFEST.into())
        .into()
}

/// Go's duration syntax for the cases a probe takes: `500ms`, `3s`, `1m`.
pub fn parse_duration(s: &str) -> Result<Duration, String> {
    let t = s.trim();
    let (num, unit) = if let Some(n) = t.strip_suffix("ms") {
        (n, 0.001)
    } else if let Some(n) = t.strip_suffix('s') {
        (n, 1.0)
    } else if let Some(n) = t.strip_suffix('m') {
        (n, 60.0)
    } else {
        return Err(format!("invalid duration {s:?}"));
    };
    let v: f64 = num.parse().map_err(|_| format!("invalid duration {s:?}"))?;
    if !v.is_finite() || v < 0.0 || num.starts_with(['+', '-']) {
        return Err(format!("invalid duration {s:?}"));
    }
    Ok(Duration::from_secs_f64(v * unit))
}

/// The address the probe dials for a listen address: a wildcard host is
/// this host.
pub fn probe_target(listen: &str) -> Result<(String, u16), String> {
    let (host, port) = match listen.rsplit_once(':') {
        Some((h, p)) if !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()) => (h, p),
        _ => {
            return Err(format!(
                "listen address {listen:?} has no fixed port to probe"
            ));
        }
    };
    let port: u16 = port
        .parse()
        .map_err(|_| format!("listen address {listen:?} has no fixed port to probe"))?;
    if port == 0 {
        return Err(format!(
            "listen address {listen:?} has no fixed port to probe"
        ));
    }
    let mut host = host
        .trim_start_matches('[')
        .trim_end_matches(']')
        .to_string();
    if host.is_empty() || host == "0.0.0.0" || host == "::" {
        host = "127.0.0.1".into();
    }
    Ok((host, port))
}

/// One health check against `listen`; `Err` names what failed.
pub async fn probe(listen: &str, env: &Env, timeout: Duration) -> Result<(), String> {
    let (host, port) = probe_target(listen)?;
    let v = env
        .get(tlsconfig::ENV_INTERNAL_TLS)
        .map(String::as_str)
        .unwrap_or("");
    tlsconfig::check(v)?;
    let mut ep = Endpoint::from_shared(format!(
        "{}://{}",
        if tlsconfig::enabled(v) {
            "https"
        } else {
            "http"
        },
        target(&host, port)
    ))
    .map_err(|e| e.to_string())?
    .connect_timeout(timeout)
    .timeout(timeout);
    if tlsconfig::enabled(v) {
        let cert = env
            .get(tlsconfig::ENV_INTERNAL_TLS_CERT)
            .cloned()
            .unwrap_or_default();
        let key = env
            .get(tlsconfig::ENV_INTERNAL_TLS_KEY)
            .cloned()
            .unwrap_or_default();
        if cert.is_empty() || key.is_empty() {
            return Err(format!(
                "{} is on but {} / {} are not both set — the listener cannot be serving TLS either",
                tlsconfig::ENV_INTERNAL_TLS,
                tlsconfig::ENV_INTERNAL_TLS_CERT,
                tlsconfig::ENV_INTERNAL_TLS_KEY
            ));
        }
        let mut forced = env.clone();
        forced.insert(tlsconfig::ENV_INTERNAL_TLS.into(), "on".into());
        let mut tls = tlsconfig::client_tls(&forced)?.unwrap_or_default();
        // The listener's certificate names this service, not 127.0.0.1:
        // verify against the container's own leaf's first DNS name.
        if let Some(name) = tlsconfig::leaf_server_name(&cert) {
            tls = tls.domain_name(name);
        }
        ep = ep.tls_config(tls).map_err(|e| e.to_string())?;
    }
    let fut = async {
        let ch = ep
            .connect()
            .await
            .map_err(|e| format!("UNAVAILABLE: {e}"))?;
        let mut client = tonic_health::pb::health_client::HealthClient::new(ch);
        let resp = client
            .check(tonic_health::pb::HealthCheckRequest {
                service: String::new(),
            })
            .await
            .map_err(|s| format!("{:?}: {}", s.code(), s.message()))?;
        let status = resp.into_inner().status();
        if status != tonic_health::pb::health_check_response::ServingStatus::Serving {
            return Err(format!("grpc.health.v1 status {}", status.as_str_name()));
        }
        Ok(())
    };
    tokio::time::timeout(timeout, fut)
        .await
        .map_err(|_| "DEADLINE_EXCEEDED: the probe timed out".to_string())?
}

const USAGE: &str = "usage: <binary> [serve|health] [options]

  serve     boot and serve the tier (default)
            --manifest PATH       w17-business.json (default $W17_BUSINESS_MANIFEST, else /app/w17-business.json)
  health    probe this tier's listener (a container HEALTHCHECK)
            --manifest PATH
            --timeout DURATION    bound on the whole probe (default 3s)
";

struct Args {
    cmd: String,
    flags: HashMap<String, String>,
}

fn parse_args(argv: &[String]) -> Result<Args, String> {
    let mut flags = HashMap::new();
    let mut cmd = String::new();
    let mut i = 0;
    while i < argv.len() {
        let a = &argv[i];
        if a == "-h" || a == "--help" {
            flags.insert("help".to_string(), "1".to_string());
        } else if let Some(f) = a.strip_prefix("--") {
            if let Some((k, v)) = f.split_once('=') {
                flags.insert(k.to_string(), v.to_string());
            } else {
                let v = argv
                    .get(i + 1)
                    .ok_or_else(|| format!("{a} needs a value"))?;
                flags.insert(f.to_string(), v.clone());
                i += 1;
            }
        } else if cmd.is_empty() {
            cmd = a.clone();
        } else {
            return Err(format!("unexpected argument {a:?}"));
        }
        i += 1;
    }
    Ok(Args {
        cmd: if cmd.is_empty() { "serve".into() } else { cmd },
        flags,
    })
}

async fn signalled() {
    #[cfg(unix)]
    {
        let mut term =
            match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
                Ok(s) => s,
                Err(_) => {
                    let _ = tokio::signal::ctrl_c().await;
                    return;
                }
            };
        tokio::select! {
            _ = term.recv() => {}
            _ = tokio::signal::ctrl_c() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}

/// The binary's whole life: what [`crate::main!`] calls. `digest` is the
/// SHA-256 of the descriptor set the crate was compiled from.
pub fn run(digest: &str, register: RegisterFn) -> ExitCode {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let argv: Vec<String> = std::env::args().skip(1).collect();
    let args = match parse_args(&argv) {
        Ok(a) => a,
        Err(e) => {
            eprintln!("w17-business: {e}\n{USAGE}");
            return ExitCode::from(2);
        }
    };
    if args.flags.contains_key("help") {
        println!("{USAGE}");
        return ExitCode::SUCCESS;
    }
    let allowed: &[&str] = match args.cmd.as_str() {
        "serve" => &["manifest"],
        "health" => &["manifest", "timeout"],
        other => {
            eprintln!("w17-business: unknown command {other:?}\n{USAGE}");
            return ExitCode::from(2);
        }
    };
    for k in args.flags.keys() {
        if !allowed.contains(&k.as_str()) {
            eprintln!("w17-business {}: unknown flag --{k}\n{USAGE}", args.cmd);
            return ExitCode::from(2);
        }
    }
    let rt = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("w17-business: start the runtime: {e}");
            return ExitCode::FAILURE;
        }
    };
    let env = secrets::process_env();
    let path = resolve_manifest_path(args.flags.get("manifest").map(String::as_str), &env);
    if args.cmd == "health" {
        let raw = args
            .flags
            .get("timeout")
            .cloned()
            .unwrap_or_else(|| "3s".into());
        let t = match parse_duration(&raw) {
            Ok(t) if !t.is_zero() => t,
            Ok(_) => {
                eprintln!("health: --timeout must be positive, got {raw}");
                return ExitCode::from(2);
            }
            Err(e) => {
                eprintln!("health: {e}");
                return ExitCode::from(2);
            }
        };
        let m = match manifest::load(&path) {
            Ok(m) => m,
            Err(e) => {
                eprintln!("w17-business: {e}");
                return ExitCode::FAILURE;
            }
        };
        let listen = env
            .get(&m.listen_env)
            .filter(|v| !v.is_empty())
            .cloned()
            .unwrap_or(m.default_listen_addr.clone());
        return match rt.block_on(probe(&listen, &env, t)) {
            Ok(()) => {
                println!("ok   grpc {listen}");
                ExitCode::SUCCESS
            }
            Err(e) => {
                println!("FAIL grpc {listen}: {e}");
                eprintln!("health: grpc not answering");
                ExitCode::FAILURE
            }
        };
    }
    let result = rt.block_on(async move {
        let m = manifest::load(&path).map_err(|e| BootError(e.0))?;
        let app = App::boot(m, env, digest, register).await?;
        app.run(signalled()).await
    });
    let code = match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("w17-business: {e}");
            ExitCode::FAILURE
        }
    };
    // After a drain everything the tier opened is closed; a library task that
    // still runs does not hold the process.
    rt.shutdown_timeout(Duration::from_secs(3));
    code
}

/// Boot an app for tests: the same steps as [`run`], on the caller's
/// runtime, serving until `stop` resolves. Returns once drained.
#[doc(hidden)]
pub async fn serve_for_test(
    manifest_path: &Path,
    env: Env,
    digest: &str,
    register: RegisterFn,
    drain_timeout: Duration,
    stop: impl std::future::Future<Output = ()>,
) -> Result<(), BootError> {
    let m = manifest::load(manifest_path).map_err(|e| BootError(e.0))?;
    let mut app = App::boot(m, env, digest, register).await?;
    app.drain_timeout = drain_timeout;
    app.run(stop).await
}
