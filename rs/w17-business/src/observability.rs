//! Logging, Sentry and OpenTelemetry, configured as Go's `core/observx`
//! configures them.
//!
//! - `W17_SENTRY_DSN` turns Sentry on (traces sampled at 0.2, server name =
//!   the service, release = `W17_SERVICE_VERSION`, environment = `W17_ENV`).
//! - `W17_OTEL_ENDPOINT` (fallback `OTEL_EXPORTER_OTLP_ENDPOINT`) turns
//!   OTLP/gRPC trace export on; `OTEL_SERVICE_NAME` overrides the service
//!   name.
//! - `W17_OTEL_TLS` — on|true|1|yes / off|false|0|no; empty follows the
//!   internal mesh (`W17_INTERNAL_TLS`), with its CA and client pair.
//! - W3C trace context (`traceparent`/`tracestate`) and baggage are the
//!   propagators, installed only when OTel export is on — as in Go.
//!
//! The trace context travels on the SDK's explicit [`Ctx`]: the server puts
//! its span's context there, and a channel injects from the context of the
//! call it sends.
//!
//! [`report_error`] is `observx.ReportError`: Sentry when on, the log always.

use std::{
    borrow::Cow,
    collections::HashMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, SystemTime},
};

use http::HeaderMap;
use opentelemetry::{
    KeyValue,
    propagation::{Extractor, Injector, TextMapCompositePropagator},
    trace::{SpanKind, Status as SpanStatus, TraceContextExt, Tracer as _},
};
use opentelemetry_sdk::{
    propagation::{BaggagePropagator, TraceContextPropagator},
    trace::{Sampler, SdkTracerProvider},
};

use crate::{ctx::Ctx, events::is_credential_key, secrets::Env, tlsconfig};

fn log(level: &str, text: &str) {
    let line = format!(
        "{} {level} w17_business: {text}",
        humantime::format_rfc3339_millis(SystemTime::now())
    );
    if level == "INFO" {
        println!("{line}");
    } else {
        eprintln!("{line}");
    }
}

/// An informational log line.
pub fn info(text: &str) {
    log("INFO", text);
}

/// A warning log line.
pub fn warn(text: &str) {
    log("WARN", text);
}

/// Report an error an operator must see, without failing anything: to
/// Sentry when it is on, and to the log always (Go falls through to
/// `log.Printf` without Sentry).
pub fn report_error(text: &str) {
    log("ERROR", &format!("error: {text}"));
    if SENTRY_ON.load(Ordering::Relaxed) {
        sentry::capture_message(text, sentry::Level::Error);
    }
}

static SENTRY_ON: AtomicBool = AtomicBool::new(false);
static OTEL_ON: AtomicBool = AtomicBool::new(false);
static STATE: Mutex<Option<State>> = Mutex::new(None);

struct State {
    sentry: Option<sentry::ClientInitGuard>,
    provider: Option<SdkTracerProvider>,
}

/// What [`init`] turned on.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Observability {
    pub sentry: bool,
    pub otel: bool,
}

const SCRUB: &str = "[scrubbed]";

fn truthy(v: &str) -> bool {
    ["on", "true", "1", "yes"].contains(&v.trim().to_ascii_lowercase().as_str())
}

fn falsy(v: &str) -> bool {
    ["off", "false", "0", "no"].contains(&v.trim().to_ascii_lowercase().as_str())
}

/// `text` with every `<field>=<value>` / `<field>: <value>` of the given
/// names redacted.
pub(crate) fn scrub_text(text: &str, fields: &[String]) -> String {
    let mut out = text.to_string();
    for f in fields {
        let mut res = String::with_capacity(out.len());
        let mut rest = out.as_str();
        while let Some(i) = rest.find(f.as_str()) {
            let (before, after) = rest.split_at(i + f.len());
            res.push_str(before);
            let trimmed = after.trim_start_matches(['"', '\'']);
            let skip = after.len() - trimmed.len();
            let sep_rest = trimmed.trim_start();
            if let Some(val) = sep_rest
                .strip_prefix('=')
                .or_else(|| sep_rest.strip_prefix(':'))
            {
                let val = val.trim_start();
                let consumed = after.len() - val.len();
                res.push_str(&after[..consumed]);
                let end = val
                    .find(|c: char| c.is_whitespace() || matches!(c, ',' | ')' | ']' | '}'))
                    .unwrap_or(val.len());
                res.push_str(SCRUB);
                rest = &val[end..];
            } else {
                res.push_str(&after[..skip]);
                rest = &after[skip..];
            }
        }
        res.push_str(rest);
        out = res;
    }
    out
}

fn scrub_event(
    mut event: sentry::protocol::Event<'static>,
    fields: &[String],
) -> sentry::protocol::Event<'static> {
    if let Some(m) = event.message.as_mut() {
        *m = scrub_text(m, fields);
    }
    for e in event.exception.values.iter_mut() {
        if let Some(v) = e.value.as_mut() {
            *v = scrub_text(v, fields);
        }
    }
    if let Some(req) = event.request.as_mut() {
        for (k, v) in req.headers.iter_mut() {
            if is_credential_key(k) || fields.iter().any(|f| f == k) {
                *v = SCRUB.to_string();
            }
        }
        if let Some(d) = req.data.as_mut() {
            *d = scrub_text(d, fields);
        }
    }
    for (k, v) in event.extra.iter_mut() {
        if is_credential_key(k) || fields.iter().any(|f| f == k) {
            *v = SCRUB.into();
        } else if let Some(s) = v.as_str() {
            *v = scrub_text(s, fields).into();
        }
    }
    event
}

/// Turn Sentry and OpenTelemetry on per the environment. A bad value (an
/// unknown `W17_OTEL_TLS`) is an error: as in Go, the only valid recovery
/// from a bad telemetry config is to fix it and restart.
pub fn init(service: &str, scrub_fields: Vec<String>, env: &Env) -> Result<Observability, String> {
    let get = |k: &str| env.get(k).cloned().unwrap_or_default();
    let service = Some(get("OTEL_SERVICE_NAME"))
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| service.to_string());
    let version = get("W17_SERVICE_VERSION");
    let environment = get("W17_ENV");
    let endpoint = Some(get("W17_OTEL_ENDPOINT"))
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| get("OTEL_EXPORTER_OTLP_ENDPOINT"));
    let mut secure = false;
    if !endpoint.is_empty() {
        let otel_tls = get("W17_OTEL_TLS");
        if !otel_tls.is_empty() && !(truthy(&otel_tls) || falsy(&otel_tls)) {
            return Err(format!(
                "W17_OTEL_TLS={otel_tls:?} is not a recognised value (on|true|1|yes, off|false|0|no)"
            ));
        }
        let mesh = get(tlsconfig::ENV_INTERNAL_TLS);
        tlsconfig::check(&mesh)?;
        secure = if otel_tls.is_empty() {
            tlsconfig::dial_tls(&mesh)
        } else {
            truthy(&otel_tls)
        };
        if falsy(&otel_tls) && tlsconfig::dial_tls(&mesh) {
            warn(&format!(
                "observability: W17_OTEL_TLS={otel_tls} exports telemetry in plaintext while {} is on",
                tlsconfig::ENV_INTERNAL_TLS
            ));
        }
    }

    let mut state = State {
        sentry: None,
        provider: None,
    };
    let dsn = get("W17_SENTRY_DSN");
    if !dsn.is_empty() {
        let dsn = dsn.parse().map_err(|e| format!("W17_SENTRY_DSN: {e}"))?;
        let fields = Arc::new(scrub_fields);
        let mut opts = sentry::ClientOptions::default();
        opts.dsn = Some(dsn);
        opts.server_name = Some(Cow::Owned(service.clone()));
        opts.release = Some(version.clone())
            .filter(|v| !v.is_empty())
            .map(Cow::Owned);
        opts.environment = Some(environment.clone())
            .filter(|v| !v.is_empty())
            .map(Cow::Owned);
        opts.traces_sampling_strategy = sentry::TracesSamplingStrategy::FixedRate(0.2);
        // Go's Sentry client sends no request bodies, cookies or IPs.
        opts.send_default_pii = false;
        opts.before_send = Some(Arc::new(move |event| Some(scrub_event(event, &fields))));
        let guard = sentry::init(opts);
        state.sentry = Some(guard);
        SENTRY_ON.store(true, Ordering::Relaxed);
    }

    if !endpoint.is_empty() {
        use opentelemetry_otlp::{WithExportConfig as _, WithTonicConfig as _};
        let url = if endpoint.contains("://") {
            endpoint.clone()
        } else {
            format!("{}://{endpoint}", if secure { "https" } else { "http" })
        };
        let mut builder = opentelemetry_otlp::SpanExporter::builder()
            .with_tonic()
            .with_endpoint(url);
        if secure {
            builder = builder.with_tls_config(tlsconfig::otlp_tls(env)?);
        }
        let exporter = builder.build().map_err(|e| format!("otel exporter: {e}"))?;
        let resource = opentelemetry_sdk::Resource::builder()
            .with_attributes([
                KeyValue::new("service.name", service.clone()),
                KeyValue::new("service.version", version.clone()),
                KeyValue::new("deployment.environment.name", environment.clone()),
            ])
            .build();
        let provider = SdkTracerProvider::builder()
            .with_batch_exporter(exporter)
            .with_resource(resource)
            .with_sampler(Sampler::ParentBased(Box::new(Sampler::TraceIdRatioBased(
                1.0,
            ))))
            .build();
        opentelemetry::global::set_tracer_provider(provider.clone());
        opentelemetry::global::set_text_map_propagator(TextMapCompositePropagator::new(vec![
            Box::new(TraceContextPropagator::new()),
            Box::new(BaggagePropagator::new()),
        ]));
        state.provider = Some(provider);
        OTEL_ON.store(true, Ordering::Relaxed);
    }
    let out = Observability {
        sentry: state.sentry.is_some(),
        otel: state.provider.is_some(),
    };
    *STATE.lock().unwrap_or_else(|e| e.into_inner()) = Some(state);
    Ok(out)
}

/// Flush and stop what [`init`] turned on, within `timeout`.
pub fn shutdown(timeout: Duration) {
    let state = STATE.lock().unwrap_or_else(|e| e.into_inner()).take();
    let Some(state) = state else { return };
    if let Some(guard) = state.sentry {
        if !guard.flush(Some(timeout)) {
            warn("sentry flush: timed out");
        }
        drop(guard);
    }
    if let Some(p) = state.provider
        && let Err(e) = p.shutdown_with_timeout(timeout)
    {
        warn(&format!("otel shutdown: {e}"));
    }
    SENTRY_ON.store(false, Ordering::Relaxed);
    OTEL_ON.store(false, Ordering::Relaxed);
}

struct HeaderExtractor<'a>(&'a HeaderMap);

impl Extractor for HeaderExtractor<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|v| v.to_str().ok())
    }

    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(|k| k.as_str()).collect()
    }
}

struct MapInjector(HashMap<String, String>);

impl Injector for MapInjector {
    fn set(&mut self, key: &str, value: String) {
        self.0.insert(key.to_string(), value);
    }
}

/// A server span for one call, when OTel export is on: the context carrying
/// it, and its end.
pub(crate) struct ServerSpan(Option<opentelemetry::Context>);

impl ServerSpan {
    pub(crate) fn end(mut self, err: Option<&str>) {
        if let Some(cx) = self.0.take() {
            let span = cx.span();
            if let Some(e) = err {
                span.set_status(SpanStatus::error(e.to_string()));
            }
            span.end();
        }
    }
}

pub(crate) fn start_server_span(ctx: Ctx, headers: &HeaderMap, path: &str) -> (Ctx, ServerSpan) {
    if !OTEL_ON.load(Ordering::Relaxed) {
        return (ctx, ServerSpan(None));
    }
    let parent =
        opentelemetry::global::get_text_map_propagator(|p| p.extract(&HeaderExtractor(headers)));
    let tracer = opentelemetry::global::tracer("w17_business");
    let span = tracer
        .span_builder(path.to_string())
        .with_kind(SpanKind::Server)
        .with_attributes([
            KeyValue::new("rpc.system", "grpc"),
            KeyValue::new("rpc.method", path.to_string()),
        ])
        .start_with_context(&tracer, &parent);
    let cx = parent.with_span(span);
    (ctx.with_trace(cx.clone()), ServerSpan(Some(cx)))
}

/// Add the trace context `ctx` carries to outgoing headers.
pub(crate) fn inject_trace(ctx: &Ctx, headers: &mut HeaderMap) {
    if !OTEL_ON.load(Ordering::Relaxed) {
        return;
    }
    let Some(cx) = ctx.trace() else { return };
    let mut inj = MapInjector(HashMap::new());
    opentelemetry::global::get_text_map_propagator(|p| p.inject_context(cx, &mut inj));
    for (k, v) in inj.0 {
        if let (Ok(name), Ok(val)) = (
            http::HeaderName::from_bytes(k.as_bytes()),
            http::HeaderValue::from_str(&v),
        ) {
            headers.insert(name, val);
        }
    }
}
