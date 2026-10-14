//! Env-driven configuration for the storage gRPC proxy.
//!
//! All knobs are read from the environment so the Go codegen (slice S4)
//! can wire them via Compose/k8s without recompiling the proxy.

use std::net::SocketAddr;
use std::time::Duration;

/// Env var: listen address (`host:port`). Default `0.0.0.0:50051`.
pub const ENV_LISTEN: &str = "W17_PROXY_LISTEN";
/// Env var: comma-separated replica `host:port` endpoints. Required.
pub const ENV_REPLICAS: &str = "W17_PROXY_REPLICAS";
/// Env var: affinity entry TTL in seconds. Default 300 (5 min,
/// aligned with the Go `distx.DefaultOrphanTimeout`).
pub const ENV_AFFINITY_TTL_SECS: &str = "W17_PROXY_AFFINITY_TTL_SECS";
/// Env var: OTLP/gRPC endpoint for metrics + (sampled) traces, e.g.
/// `http://w17-tracing:4317`. Unset/empty → telemetry pipeline is never
/// built (zero per-RPC overhead). Same convention as the rest of the
/// project (`W17_OTEL_ENDPOINT`).
pub const ENV_OTEL_ENDPOINT: &str = "W17_OTEL_ENDPOINT";
/// Env var: per-RPC trace sampling ratio in `[0,1]`. Default `0.0` (no
/// per-RPC spans). Only effective when `W17_OTEL_ENDPOINT` is also set.
pub const ENV_TRACE_SAMPLE_RATIO: &str = "W17_PROXY_TRACE_SAMPLE_RATIO";

// --- Internal-mesh TLS (stack-wide switch) ---------------------------
// The proxy sits ENTIRELY inside the internal mesh: clients (the gateway)
// dial it, and it re-dials the storage backends. Both legs share ONE
// stack-wide switch — the same `W17_INTERNAL_TLS` the Go services read
// (go/lib/grpcclient) — because encrypting the internal mesh is an
// all-or-nothing infra decision, not a per-component knob: a mesh half
// encrypted is a mesh an attacker on it can still read. Off by default
// (plain h2c both legs): the mesh is a trusted network the platform
// secures.
/// Env var: stack-wide switch. `on`/`true`/`1`/`yes` turns internal TLS on
/// for BOTH legs; unset or anything else = off (plain h2c), the default.
pub const ENV_INTERNAL_TLS: &str = "W17_INTERNAL_TLS";
/// Env var: PEM path to THIS proxy's leaf cert. Required when the switch is
/// on — presented as the server cert (client leg) and as the mTLS client
/// cert (backend leg).
pub const ENV_TLS_CERT: &str = "W17_INTERNAL_TLS_CERT";
/// Env var: PEM path to this proxy's leaf key (pairs with `_CERT`).
pub const ENV_TLS_KEY: &str = "W17_INTERNAL_TLS_KEY";
/// Env var: PEM CA the proxy uses to verify the BACKEND leaves (client leg
/// RootCAs). Unset = system trust roots. There is no skip-verify: when TLS
/// is on, verification is always real (dev PKI leaves carry service SANs).
pub const ENV_TLS_CA: &str = "W17_INTERNAL_TLS_CA";
/// Env var: PEM CA the proxy uses to verify CLIENT certs on the SERVER leg —
/// opt-in mutual TLS. Unset = server-auth TLS only.
pub const ENV_TLS_CLIENT_CA: &str = "W17_INTERNAL_TLS_CLIENT_CA";

const DEFAULT_LISTEN: &str = "0.0.0.0:50051";
const DEFAULT_TTL_SECS: u64 = 300;
const DEFAULT_TRACE_SAMPLE_RATIO: f64 = 0.0;

/// Server-leg TLS material (the proxy listens for client TLS).
/// `None` on [`Config`] ⇒ plain h2c listener (mesh opt-out / debug).
#[derive(Debug, Clone)]
pub struct ServerTls {
    pub cert_path: String,
    pub key_path: String,
    /// mTLS: when set, require + verify client certs against this CA.
    /// `None` ⇒ server-auth TLS only.
    pub client_ca_path: Option<String>,
}

/// Client-leg TLS posture (the proxy dials backends over TLS).
/// `None` on [`Config`] ⇒ plain h2c dial (`http://`).
#[derive(Debug, Clone)]
pub struct BackendTls {
    /// Pinned CA to verify the backend leaf against. `None` ⇒ verify
    /// against the system roots. Verification is ALWAYS on (no skip-verify).
    pub ca_path: Option<String>,
    /// mTLS client identity `(cert_path, key_path)` — this proxy's leaf,
    /// presented so a zero-trust backend can verify the proxy too.
    pub client_identity: Option<(String, String)>,
}

/// Resolved, validated proxy configuration.
#[derive(Debug, Clone)]
pub struct Config {
    pub listen: SocketAddr,
    /// Replica authorities (`host:port`), in declared order. Non-empty.
    pub replicas: Vec<String>,
    pub affinity_ttl: Duration,
    /// OTLP/gRPC endpoint. `None`/empty ⇒ telemetry disabled entirely.
    pub otel_endpoint: Option<String>,
    /// Per-RPC span sampling ratio in `[0,1]`. `0.0` ⇒ no per-RPC spans.
    pub trace_sample_ratio: f64,
    /// Server-leg TLS (clients → proxy). `None` ⇒ plain h2c listener.
    pub server_tls: Option<ServerTls>,
    /// Client-leg TLS (proxy → backends). `None` ⇒ plain h2c dial.
    pub backend_tls: Option<BackendTls>,
}

impl Config {
    /// True when an OTLP endpoint is configured (metrics export on).
    pub fn otel_enabled(&self) -> bool {
        self.otel_endpoint.is_some()
    }

    /// True when per-RPC spans should be created (endpoint set AND a
    /// positive sample ratio).
    pub fn tracing_enabled(&self) -> bool {
        self.otel_enabled() && self.trace_sample_ratio > 0.0
    }

    /// URI scheme for dialing backends — `https` when the client leg uses
    /// TLS, else `http`.
    pub fn backend_scheme(&self) -> &'static str {
        if self.backend_tls.is_some() {
            "https"
        } else {
            "http"
        }
    }
}

/// A typed view over the process environment so config parsing can be
/// unit-tested without touching real env vars.
pub trait EnvSource {
    fn get(&self, key: &str) -> Option<String>;
}

/// Reads from the real process environment.
pub struct ProcessEnv;

impl EnvSource for ProcessEnv {
    fn get(&self, key: &str) -> Option<String> {
        std::env::var(key).ok()
    }
}

impl Config {
    /// Load + validate config from the process environment.
    pub fn from_env() -> Result<Config, String> {
        Config::from_source(&ProcessEnv)
    }

    /// Load + validate config from an arbitrary [`EnvSource`].
    pub fn from_source(env: &dyn EnvSource) -> Result<Config, String> {
        let listen = parse_listen(env)?;
        let replicas = parse_replicas(env)?;
        let affinity_ttl = parse_ttl(env)?;
        let otel_endpoint = parse_otel_endpoint(env);
        let trace_sample_ratio = parse_trace_sample_ratio(env)?;
        let server_tls = parse_server_tls(env)?;
        let backend_tls = parse_backend_tls(env)?;
        Ok(Config {
            listen,
            replicas,
            affinity_ttl,
            otel_endpoint,
            trace_sample_ratio,
            server_tls,
            backend_tls,
        })
    }
}

/// The stack-wide internal-TLS switch: on for `on`/`true`/`1`/`yes`
/// (case-insensitive), off otherwise (incl. unset). Same spelling as the
/// Go `grpcclient.InternalTLSEnabled`.
fn internal_tls_on(env: &dyn EnvSource) -> bool {
    match env.get(ENV_INTERNAL_TLS) {
        Some(v) => matches!(
            v.trim().to_ascii_lowercase().as_str(),
            "on" | "true" | "1" | "yes"
        ),
        None => false,
    }
}

/// Trimmed non-empty env value, else `None`.
fn nonempty(env: &dyn EnvSource, key: &str) -> Option<String> {
    env.get(key)
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// This proxy's leaf `(cert, key)` — both-or-neither. Presented as the
/// server cert (client leg) and the mTLS client cert (backend leg).
fn node_leaf(env: &dyn EnvSource) -> Result<Option<(String, String)>, String> {
    match (nonempty(env, ENV_TLS_CERT), nonempty(env, ENV_TLS_KEY)) {
        (Some(c), Some(k)) => Ok(Some((c, k))),
        (None, None) => Ok(None),
        _ => Err(format!(
            "{ENV_TLS_CERT} and {ENV_TLS_KEY} must be set together (a leaf needs both)"
        )),
    }
}

/// Server-leg TLS: OFF unless the stack-wide `W17_INTERNAL_TLS` switch is on.
/// When on, this proxy's leaf cert+key are required (a missing pair fails
/// fast at startup, never at first handshake).
fn parse_server_tls(env: &dyn EnvSource) -> Result<Option<ServerTls>, String> {
    if !internal_tls_on(env) {
        return Ok(None);
    }
    let (cert_path, key_path) = node_leaf(env)?.ok_or_else(|| {
        format!(
            "{ENV_INTERNAL_TLS}=on but {ENV_TLS_CERT} / {ENV_TLS_KEY} are not both set — \
             point them at this proxy's leaf cert+key (dev PKI: w17/certs/dev.local/)"
        )
    })?;
    Ok(Some(ServerTls {
        cert_path,
        key_path,
        client_ca_path: nonempty(env, ENV_TLS_CLIENT_CA),
    }))
}

/// Client-leg TLS: OFF unless the stack-wide `W17_INTERNAL_TLS` switch is on.
/// When on, the proxy verifies backend leaves (against `_CA` or system roots)
/// and presents its own leaf as the mTLS client cert.
fn parse_backend_tls(env: &dyn EnvSource) -> Result<Option<BackendTls>, String> {
    if !internal_tls_on(env) {
        return Ok(None);
    }
    Ok(Some(BackendTls {
        ca_path: nonempty(env, ENV_TLS_CA),
        client_identity: node_leaf(env)?,
    }))
}

/// OTLP endpoint: trimmed; empty/whitespace ⇒ `None` (disabled).
fn parse_otel_endpoint(env: &dyn EnvSource) -> Option<String> {
    env.get(ENV_OTEL_ENDPOINT)
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// Trace sample ratio: float in `[0,1]`, default `0.0`. Out-of-range or
/// non-numeric values are a clear config error.
fn parse_trace_sample_ratio(env: &dyn EnvSource) -> Result<f64, String> {
    match env.get(ENV_TRACE_SAMPLE_RATIO) {
        None => Ok(DEFAULT_TRACE_SAMPLE_RATIO),
        Some(raw) if raw.trim().is_empty() => Ok(DEFAULT_TRACE_SAMPLE_RATIO),
        Some(raw) => {
            let ratio = raw
                .trim()
                .parse::<f64>()
                .map_err(|e| format!("{ENV_TRACE_SAMPLE_RATIO}: invalid float {raw:?}: {e}"))?;
            if !ratio.is_finite() || !(0.0..=1.0).contains(&ratio) {
                return Err(format!(
                    "{ENV_TRACE_SAMPLE_RATIO} must be a float in [0,1] (got {raw:?})"
                ));
            }
            Ok(ratio)
        }
    }
}

fn parse_listen(env: &dyn EnvSource) -> Result<SocketAddr, String> {
    let raw = env
        .get(ENV_LISTEN)
        .unwrap_or_else(|| DEFAULT_LISTEN.to_string());
    raw.parse::<SocketAddr>()
        .map_err(|e| format!("{ENV_LISTEN}: invalid listen address {raw:?}: {e}"))
}

fn parse_replicas(env: &dyn EnvSource) -> Result<Vec<String>, String> {
    let raw = env.get(ENV_REPLICAS).unwrap_or_default();
    let replicas: Vec<String> = raw
        .split(',')
        .map(|s| s.trim())
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .collect();
    if replicas.is_empty() {
        return Err(format!(
            "{ENV_REPLICAS} is required and must list at least one replica \
             host:port (got {raw:?})"
        ));
    }
    Ok(replicas)
}

fn parse_ttl(env: &dyn EnvSource) -> Result<Duration, String> {
    match env.get(ENV_AFFINITY_TTL_SECS) {
        None => Ok(Duration::from_secs(DEFAULT_TTL_SECS)),
        Some(raw) => {
            let secs = raw
                .trim()
                .parse::<u64>()
                .map_err(|e| format!("{ENV_AFFINITY_TTL_SECS}: invalid u64 {raw:?}: {e}"))?;
            if secs == 0 {
                return Err(format!("{ENV_AFFINITY_TTL_SECS} must be > 0"));
            }
            Ok(Duration::from_secs(secs))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    struct MapEnv(HashMap<String, String>);
    impl EnvSource for MapEnv {
        fn get(&self, key: &str) -> Option<String> {
            self.0.get(key).cloned()
        }
    }
    /// Builds an env from the given pairs. Internal TLS is OFF unless a test
    /// seeds `W17_INTERNAL_TLS=on`, so the non-TLS field tests stay plain-h2c
    /// with nothing to seed. `raw` is an alias kept for the TLS tests that
    /// want to read as "no switch seeded".
    fn env(pairs: &[(&str, &str)]) -> MapEnv {
        MapEnv(
            pairs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
        )
    }
    fn raw(pairs: &[(&str, &str)]) -> MapEnv {
        env(pairs)
    }

    #[test]
    fn defaults_apply_when_only_replicas_set() {
        let c = Config::from_source(&env(&[(ENV_REPLICAS, "a:1,b:2")])).unwrap();
        assert_eq!(c.listen.to_string(), "0.0.0.0:50051");
        assert_eq!(c.replicas, vec!["a:1", "b:2"]);
        assert_eq!(c.affinity_ttl, Duration::from_secs(300));
    }

    #[test]
    fn replicas_required() {
        let err = Config::from_source(&env(&[])).unwrap_err();
        assert!(err.contains(ENV_REPLICAS), "got: {err}");
    }

    #[test]
    fn replicas_whitespace_and_empties_trimmed() {
        let c = Config::from_source(&env(&[(ENV_REPLICAS, " a:1 , , b:2 ,")])).unwrap();
        assert_eq!(c.replicas, vec!["a:1", "b:2"]);
    }

    #[test]
    fn all_empty_replicas_rejected() {
        let err = Config::from_source(&env(&[(ENV_REPLICAS, " , , ")])).unwrap_err();
        assert!(err.contains(ENV_REPLICAS), "got: {err}");
    }

    #[test]
    fn custom_listen_and_ttl() {
        let c = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_LISTEN, "127.0.0.1:6000"),
            (ENV_AFFINITY_TTL_SECS, "60"),
        ]))
        .unwrap();
        assert_eq!(c.listen.to_string(), "127.0.0.1:6000");
        assert_eq!(c.affinity_ttl, Duration::from_secs(60));
    }

    #[test]
    fn bad_listen_rejected() {
        let err =
            Config::from_source(&env(&[(ENV_REPLICAS, "a:1"), (ENV_LISTEN, "nope")])).unwrap_err();
        assert!(err.contains(ENV_LISTEN), "got: {err}");
    }

    #[test]
    fn zero_ttl_rejected() {
        let err = Config::from_source(&env(&[(ENV_REPLICAS, "a:1"), (ENV_AFFINITY_TTL_SECS, "0")]))
            .unwrap_err();
        assert!(err.contains(ENV_AFFINITY_TTL_SECS), "got: {err}");
    }

    #[test]
    fn non_numeric_ttl_rejected() {
        let err = Config::from_source(&env(&[(ENV_REPLICAS, "a:1"), (ENV_AFFINITY_TTL_SECS, "x")]))
            .unwrap_err();
        assert!(err.contains(ENV_AFFINITY_TTL_SECS), "got: {err}");
    }

    // ---- OTEL config ------------------------------------------------

    #[test]
    fn otel_disabled_by_default() {
        let c = Config::from_source(&env(&[(ENV_REPLICAS, "a:1")])).unwrap();
        assert_eq!(c.otel_endpoint, None);
        assert_eq!(c.trace_sample_ratio, 0.0);
        assert!(!c.otel_enabled());
        assert!(!c.tracing_enabled());
    }

    #[test]
    fn empty_otel_endpoint_is_disabled() {
        let c =
            Config::from_source(&env(&[(ENV_REPLICAS, "a:1"), (ENV_OTEL_ENDPOINT, "  ")])).unwrap();
        assert_eq!(c.otel_endpoint, None);
        assert!(!c.otel_enabled());
    }

    #[test]
    fn otel_endpoint_trimmed_and_kept() {
        let c = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_OTEL_ENDPOINT, " http://w17-tracing:4317 "),
        ]))
        .unwrap();
        assert_eq!(c.otel_endpoint.as_deref(), Some("http://w17-tracing:4317"));
        assert!(c.otel_enabled());
        // endpoint without a ratio ⇒ metrics on, per-RPC spans off.
        assert!(!c.tracing_enabled());
    }

    #[test]
    fn trace_ratio_defaults_to_zero() {
        let c = Config::from_source(&env(&[(ENV_REPLICAS, "a:1")])).unwrap();
        assert_eq!(c.trace_sample_ratio, 0.0);
    }

    #[test]
    fn trace_ratio_parsed() {
        let c = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_OTEL_ENDPOINT, "http://collector:4317"),
            (ENV_TRACE_SAMPLE_RATIO, "0.25"),
        ]))
        .unwrap();
        assert_eq!(c.trace_sample_ratio, 0.25);
        assert!(c.tracing_enabled());
    }

    #[test]
    fn trace_ratio_one_and_zero_ok() {
        let c1 = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "1"),
        ]))
        .unwrap();
        assert_eq!(c1.trace_sample_ratio, 1.0);
        let c0 = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "0"),
        ]))
        .unwrap();
        assert_eq!(c0.trace_sample_ratio, 0.0);
    }

    #[test]
    fn trace_ratio_negative_rejected() {
        let err = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "-0.1"),
        ]))
        .unwrap_err();
        assert!(err.contains(ENV_TRACE_SAMPLE_RATIO), "got: {err}");
    }

    #[test]
    fn trace_ratio_above_one_rejected() {
        let err = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "1.5"),
        ]))
        .unwrap_err();
        assert!(err.contains(ENV_TRACE_SAMPLE_RATIO), "got: {err}");
    }

    #[test]
    fn trace_ratio_non_numeric_rejected() {
        let err = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "nope"),
        ]))
        .unwrap_err();
        assert!(err.contains(ENV_TRACE_SAMPLE_RATIO), "got: {err}");
    }

    #[test]
    fn tracing_enabled_requires_endpoint() {
        // ratio set but no endpoint ⇒ tracing stays off (nothing to export to).
        let c = Config::from_source(&env(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_TRACE_SAMPLE_RATIO, "1"),
        ]))
        .unwrap();
        assert!(!c.tracing_enabled());
    }

    // ---- Internal-mesh TLS: OFF by default --------------------------

    #[test]
    fn tls_off_by_default_both_legs() {
        // Nothing seeded ⇒ switch off ⇒ both legs plain h2c, no cert needed.
        let c = Config::from_source(&raw(&[(ENV_REPLICAS, "a:1")])).unwrap();
        assert!(c.server_tls.is_none(), "server leg plain by default");
        assert!(c.backend_tls.is_none(), "backend leg plain by default");
        assert_eq!(c.backend_scheme(), "http");
    }

    #[test]
    fn tls_off_when_switch_false() {
        let c = Config::from_source(&raw(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_INTERNAL_TLS, "false"),
            // cert/key present but ignored while the switch is off.
            (ENV_TLS_CERT, "/certs/server.crt"),
            (ENV_TLS_KEY, "/certs/server.key"),
        ]))
        .unwrap();
        assert!(c.server_tls.is_none());
        assert!(c.backend_tls.is_none());
    }

    // ---- Internal-mesh TLS: ON (switch = on) ------------------------

    #[test]
    fn tls_on_requires_leaf_cert_and_key() {
        // Switch on but no leaf ⇒ fail fast (server leg parses first).
        let err = Config::from_source(&raw(&[(ENV_REPLICAS, "a:1"), (ENV_INTERNAL_TLS, "on")]))
            .unwrap_err();
        assert!(err.contains(ENV_TLS_CERT), "got: {err}");
    }

    #[test]
    fn tls_on_only_cert_set_rejected() {
        let err = Config::from_source(&raw(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_INTERNAL_TLS, "on"),
            (ENV_TLS_CERT, "/certs/server.crt"),
        ]))
        .unwrap_err();
        assert!(err.contains(ENV_TLS_KEY), "got: {err}");
    }

    #[test]
    fn tls_on_both_legs_from_one_leaf() {
        // Switch on + leaf ⇒ BOTH legs light up from the single leaf. No
        // skip-verify field exists; backend verification is always real.
        let c = Config::from_source(&raw(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_INTERNAL_TLS, "on"),
            (ENV_TLS_CERT, "/certs/server.crt"),
            (ENV_TLS_KEY, "/certs/server.key"),
        ]))
        .unwrap();
        let s = c.server_tls.as_ref().expect("server_tls present");
        assert_eq!(s.cert_path, "/certs/server.crt");
        assert_eq!(s.key_path, "/certs/server.key");
        assert!(s.client_ca_path.is_none(), "server-auth only by default");

        assert_eq!(c.backend_scheme(), "https");
        let b = c.backend_tls.as_ref().expect("backend_tls present");
        assert!(b.ca_path.is_none(), "system roots by default");
        // The proxy presents its own leaf as the mTLS client cert.
        assert_eq!(
            b.client_identity,
            Some((
                "/certs/server.crt".to_string(),
                "/certs/server.key".to_string()
            ))
        );
    }

    #[test]
    fn tls_on_server_mtls_client_ca_and_backend_ca_carried() {
        let c = Config::from_source(&raw(&[
            (ENV_REPLICAS, "a:1"),
            (ENV_INTERNAL_TLS, "on"),
            (ENV_TLS_CERT, "/certs/server.crt"),
            (ENV_TLS_KEY, "/certs/server.key"),
            (ENV_TLS_CLIENT_CA, "/certs/ca.crt"),
            (ENV_TLS_CA, "/certs/ca.crt"),
        ]))
        .unwrap();
        assert_eq!(
            c.server_tls.unwrap().client_ca_path.as_deref(),
            Some("/certs/ca.crt")
        );
        assert_eq!(
            c.backend_tls.unwrap().ca_path.as_deref(),
            Some("/certs/ca.crt")
        );
    }
}
