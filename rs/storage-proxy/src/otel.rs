//! OpenTelemetry profile for the proxy — provider setup + cheap
//! instrument handles. Three tiers:
//!
//! 1. **Trace-context propagation** — always on, lives entirely in the
//!    forward path (headers are copied verbatim, including W3C
//!    `traceparent`/`tracestate`). Nothing in this module is needed for
//!    it; see `tests/integration.rs::traceparent_is_forwarded`.
//! 2. **Metrics** — always on *when an OTLP endpoint is configured*.
//!    Instruments are created ONCE here; recording on the hot path is a
//!    cheap atomic add (counter/histogram) or a no-op when disabled.
//! 3. **Per-RPC spans** — opt-in + sampled (`TraceIdRatioBased`). Off by
//!    default; only built when endpoint set AND ratio > 0. When off, the
//!    [`Telemetry::maybe_span`] entry point short-circuits with no work.
//!
//! NONE of this reads message bodies — metrics/spans carry only routing
//! metadata (class / reason / replica index / grpc status), never the
//! `:path` or `conn_id` (low cardinality + no caller data).

use std::sync::Arc;
use std::time::Duration;

use opentelemetry::metrics::{Counter, Histogram, Meter, MeterProvider as _};
use opentelemetry::trace::{SpanKind, Tracer, TracerProvider as _};
use opentelemetry::{global, Context, KeyValue};
use opentelemetry_otlp::WithExportConfig;
use opentelemetry_sdk::metrics::SdkMeterProvider;
use opentelemetry_sdk::trace::{Sampler, TracerProvider};
use opentelemetry_sdk::Resource;

use crate::config::Config;
use crate::router::Class;

const SERVICE_NAME: &str = "storage-proxy";

/// Owns the SDK providers so the process can flush + shut them down
/// gracefully on SIGINT. Holds the cheap [`Metrics`] handle and (when
/// per-RPC tracing is enabled) a sampling [`ProxyTracer`].
pub struct Telemetry {
    meter_provider: Option<SdkMeterProvider>,
    tracer_provider: Option<TracerProvider>,
    metrics: Option<Metrics>,
    tracer: Option<ProxyTracer>,
}

impl Telemetry {
    /// Build the telemetry pipeline from config. When no OTLP endpoint is
    /// configured this returns a fully-disabled handle and NEVER touches
    /// the OTLP exporter machinery (zero overhead). On exporter-build
    /// failure it logs and degrades to disabled rather than aborting the
    /// proxy (telemetry must never take routing down).
    pub fn from_config(cfg: &Config, affinity_len: AffinityLen) -> Telemetry {
        let Some(endpoint) = cfg.otel_endpoint.clone() else {
            tracing::info!(
                "otel: disabled (W17_OTEL_ENDPOINT unset) — metrics + per-RPC spans off"
            );
            return Telemetry::disabled();
        };

        let resource = Resource::new(vec![KeyValue::new("service.name", SERVICE_NAME)]);

        // --- metrics (always on when endpoint set) ----------------------
        let (meter_provider, metrics) = match build_meter_provider(
            &endpoint,
            resource.clone(),
            cfg.replicas.len(),
            affinity_len,
        ) {
            Ok((mp, meter)) => {
                let m = Metrics::new(&meter);
                (Some(mp), Some(m))
            }
            Err(e) => {
                tracing::error!(error = %e, "otel: metrics pipeline failed to build — metrics disabled");
                (None, None)
            }
        };

        // --- per-RPC spans (opt-in + sampled) ---------------------------
        let (tracer_provider, tracer) = if cfg.trace_sample_ratio > 0.0 {
            match build_tracer_provider(&endpoint, resource, cfg.trace_sample_ratio) {
                Ok(tp) => {
                    let t = tp.tracer(SERVICE_NAME);
                    (
                        Some(tp),
                        Some(ProxyTracer {
                            tracer: Arc::new(t),
                        }),
                    )
                }
                Err(e) => {
                    tracing::error!(error = %e, "otel: trace pipeline failed to build — per-RPC spans disabled");
                    (None, None)
                }
            }
        } else {
            (None, None)
        };

        tracing::info!(
            endpoint = %endpoint,
            metrics = metrics.is_some(),
            per_rpc_spans = tracer.is_some(),
            trace_sample_ratio = cfg.trace_sample_ratio,
            "otel: enabled"
        );

        Telemetry {
            meter_provider,
            tracer_provider,
            metrics,
            tracer,
        }
    }

    fn disabled() -> Telemetry {
        Telemetry {
            meter_provider: None,
            tracer_provider: None,
            metrics: None,
            tracer: None,
        }
    }

    /// The cheap, clonable metrics handle to thread into the forward path.
    /// `None` ⇒ disabled ⇒ every record call is a no-op.
    pub fn metrics(&self) -> Option<Metrics> {
        self.metrics.clone()
    }

    /// The sampling tracer, if per-RPC spans are enabled.
    pub fn tracer(&self) -> Option<ProxyTracer> {
        self.tracer.clone()
    }

    /// Flush + shut down the providers so the last metrics/spans export.
    /// Safe to call when disabled (no-op).
    pub fn shutdown(self) {
        if let Some(mp) = self.meter_provider {
            if let Err(e) = mp.force_flush() {
                tracing::warn!(error = %e, "otel: meter flush failed");
            }
            if let Err(e) = mp.shutdown() {
                tracing::warn!(error = %e, "otel: meter shutdown failed");
            }
        }
        if let Some(tp) = self.tracer_provider {
            for r in tp.force_flush() {
                if let Err(e) = r {
                    tracing::warn!(error = %e, "otel: tracer flush failed");
                }
            }
            if let Err(e) = tp.shutdown() {
                tracing::warn!(error = %e, "otel: tracer shutdown failed");
            }
        }
        global::shutdown_tracer_provider();
    }
}

/// A closure reading the current affinity map size for the observable
/// gauge. Boxed so the server can wire its `AffinityMap` without `otel`
/// depending on it.
pub type AffinityLen = Arc<dyn Fn() -> u64 + Send + Sync>;

fn build_meter_provider(
    endpoint: &str,
    resource: Resource,
    replica_count: usize,
    affinity_len: AffinityLen,
) -> Result<(SdkMeterProvider, Meter), Box<dyn std::error::Error>> {
    let exporter = opentelemetry_otlp::MetricExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint)
        .with_timeout(Duration::from_secs(5))
        .build()?;

    let reader = opentelemetry_sdk::metrics::PeriodicReader::builder(
        exporter,
        opentelemetry_sdk::runtime::Tokio,
    )
    .with_interval(Duration::from_secs(15))
    .build();

    let provider = SdkMeterProvider::builder()
        .with_reader(reader)
        .with_resource(resource)
        .build();

    let meter = provider.meter(SERVICE_NAME);

    // Observable gauge: current affinity map size (read at collection time).
    let _affinity_gauge = meter
        .u64_observable_gauge("w17.proxy.affinity_map_size")
        .with_description("current number of live conn_id->replica affinity entries")
        .with_callback(move |obs| obs.observe(affinity_len(), &[]))
        .build();

    // Gauge: configured replica count (cheap constant).
    let n = replica_count as u64;
    let _replicas_gauge = meter
        .u64_observable_gauge("w17.proxy.replicas")
        .with_description("number of configured storage replicas")
        .with_callback(move |obs| obs.observe(n, &[]))
        .build();

    Ok((provider, meter))
}

fn build_tracer_provider(
    endpoint: &str,
    resource: Resource,
    ratio: f64,
) -> Result<TracerProvider, Box<dyn std::error::Error>> {
    let exporter = opentelemetry_otlp::SpanExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint)
        .with_timeout(Duration::from_secs(5))
        .build()?;

    // Parent-based ratio sampler: if the incoming traceparent is sampled
    // we always record (so the gateway→storage trace stays intact); when
    // the proxy is the root, sample at the configured ratio.
    let sampler = Sampler::ParentBased(Box::new(Sampler::TraceIdRatioBased(ratio)));

    let provider = TracerProvider::builder()
        .with_batch_exporter(exporter, opentelemetry_sdk::runtime::Tokio)
        .with_sampler(sampler)
        .with_resource(resource)
        .build();

    Ok(provider)
}

/// Cheap, clonable bundle of metric instruments. `Arc`-shared so cloning
/// into every connection task is a refcount bump. Recording is a single
/// atomic add (counter) / histogram update — no allocation per RPC beyond
/// the small attribute array.
#[derive(Clone)]
pub struct Metrics {
    inner: Arc<MetricsInner>,
}

struct MetricsInner {
    routed_total: Counter<u64>,
    route_errors_total: Counter<u64>,
    forward_duration_ms: Histogram<f64>,
}

impl Metrics {
    fn new(meter: &Meter) -> Metrics {
        let routed_total = meter
            .u64_counter("w17.proxy.routed_total")
            .with_description("RPCs routed, by class")
            .build();
        let route_errors_total = meter
            .u64_counter("w17.proxy.route_errors_total")
            .with_description("routing errors, by reason")
            .build();
        let forward_duration_ms = meter
            .f64_histogram("w17.proxy.forward_duration_ms")
            .with_description("upstream forward latency in milliseconds, by class")
            .with_unit("ms")
            .build();
        Metrics {
            inner: Arc::new(MetricsInner {
                routed_total,
                route_errors_total,
                forward_duration_ms,
            }),
        }
    }

    /// Count one routed RPC for `class`.
    pub fn record_routed(&self, class: Class) {
        self.inner
            .routed_total
            .add(1, &[KeyValue::new("class", class_label(class))]);
    }

    /// Count one routing error with `reason` (a small enum label).
    pub fn record_route_error(&self, reason: &'static str) {
        self.inner
            .route_errors_total
            .add(1, &[KeyValue::new("reason", reason)]);
    }

    /// Record an upstream forward latency in milliseconds for `class`.
    pub fn record_forward_duration(&self, class: Class, ms: f64) {
        self.inner
            .forward_duration_ms
            .record(ms, &[KeyValue::new("class", class_label(class))]);
    }
}

/// Extension so the forward path can call the same methods whether or not
/// metrics are enabled: `Option<&Metrics>` records or no-ops.
pub trait MetricsExt {
    fn routed(&self, class: Class);
    fn route_error(&self, reason: &'static str);
    fn forward_duration(&self, class: Class, ms: f64);
}

impl MetricsExt for Option<Metrics> {
    fn routed(&self, class: Class) {
        if let Some(m) = self {
            m.record_routed(class);
        }
    }
    fn route_error(&self, reason: &'static str) {
        if let Some(m) = self {
            m.record_route_error(reason);
        }
    }
    fn forward_duration(&self, class: Class, ms: f64) {
        if let Some(m) = self {
            m.record_forward_duration(class, ms);
        }
    }
}

/// The sampling tracer. Cheap to clone (Arc). `maybe_span` is the single
/// hot-path entry point — it is only ever reached when tracing is enabled
/// (the caller holds `Option<ProxyTracer>`); the SDK sampler then decides
/// whether the span is recorded/exported.
#[derive(Clone)]
pub struct ProxyTracer {
    tracer: Arc<opentelemetry_sdk::trace::Tracer>,
}

impl ProxyTracer {
    /// Start a span for one forwarded RPC, parented to `parent` (the
    /// context extracted from the incoming `traceparent`). Returns a guard
    /// whose `finish(grpc_status)` sets the result status + ends the span.
    pub fn start(&self, class: Class, parent: &Context) -> ProxySpan {
        use opentelemetry::trace::Span;
        let mut span = self
            .tracer
            .span_builder(format!("storage-proxy {}", class_label(class)))
            .with_kind(SpanKind::Client)
            .start_with_context(self.tracer.as_ref(), parent);
        span.set_attribute(KeyValue::new("w17.proxy.class", class_label(class)));
        ProxySpan { span }
    }
}

/// An in-flight per-RPC span. Sets `w17.proxy.replica` + `grpc.status` on
/// finish.
pub struct ProxySpan {
    span: opentelemetry_sdk::trace::Span,
}

impl ProxySpan {
    /// Record the chosen replica index on the span.
    pub fn set_replica(&mut self, replica: usize) {
        use opentelemetry::trace::Span;
        self.span
            .set_attribute(KeyValue::new("w17.proxy.replica", replica as i64));
    }

    /// Set the resulting gRPC status and end the span.
    pub fn finish(mut self, grpc_status: i32) {
        use opentelemetry::trace::Span;
        self.span
            .set_attribute(KeyValue::new("grpc.status", grpc_status as i64));
        self.span.end();
    }
}

/// Extract a W3C trace context from request headers (the incoming
/// `traceparent`/`tracestate`) so the proxy span joins the existing trace.
pub fn extract_parent(headers: &http::HeaderMap) -> Context {
    let extractor = HeaderExtractor(headers);
    global::get_text_map_propagator(|p| p.extract(&extractor))
}

/// Install the W3C trace-context propagator globally so [`extract_parent`]
/// understands `traceparent`/`tracestate`. Idempotent; cheap.
pub fn install_propagator() {
    global::set_text_map_propagator(opentelemetry_sdk::propagation::TraceContextPropagator::new());
}

struct HeaderExtractor<'a>(&'a http::HeaderMap);
impl opentelemetry::propagation::Extractor for HeaderExtractor<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|v| v.to_str().ok())
    }
    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(|k| k.as_str()).collect()
    }
}

/// Low-cardinality label for a request class (a small fixed enum — safe
/// as a metric/span attribute).
fn class_label(class: Class) -> &'static str {
    match class {
        Class::Begin => "begin",
        Class::InTx => "in_tx",
        Class::Finish => "finish",
        Class::LoadBalanced => "load_balanced",
        Class::UnknownConn => "unknown_conn",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use opentelemetry::trace::TraceContextExt;

    #[test]
    fn class_labels_are_stable_low_cardinality() {
        assert_eq!(class_label(Class::Begin), "begin");
        assert_eq!(class_label(Class::InTx), "in_tx");
        assert_eq!(class_label(Class::Finish), "finish");
        assert_eq!(class_label(Class::LoadBalanced), "load_balanced");
        assert_eq!(class_label(Class::UnknownConn), "unknown_conn");
    }

    #[test]
    fn disabled_metrics_handle_is_noop() {
        // `None` handle: every record method must be a safe no-op.
        let m: Option<Metrics> = None;
        m.routed(Class::Begin);
        m.route_error("unknown_conn");
        m.forward_duration(Class::InTx, 1.5);
        // No panic == pass.
    }

    #[test]
    fn extract_parent_without_traceparent_is_invalid() {
        install_propagator();
        let headers = http::HeaderMap::new();
        let cx = extract_parent(&headers);
        // No incoming traceparent ⇒ no valid remote span context.
        assert!(!cx.span().span_context().is_valid());
    }

    #[test]
    fn extract_parent_reads_traceparent() {
        install_propagator();
        let mut headers = http::HeaderMap::new();
        headers.insert(
            "traceparent",
            http::HeaderValue::from_static(
                "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
            ),
        );
        let cx = extract_parent(&headers);
        let span = cx.span();
        let sc = span.span_context();
        assert!(sc.is_valid(), "extracted remote context should be valid");
        assert_eq!(
            sc.trace_id().to_string(),
            "4bf92f3577b34da6a3ce929d0e0e4736"
        );
    }
}
