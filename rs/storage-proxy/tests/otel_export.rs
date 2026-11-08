//! The OTLP export path, end to end: `Telemetry` built from config against an
//! in-process OTLP/gRPC receiver, one metric and one sampled span recorded,
//! then `shutdown()` — the receiver must have been sent both.
//!
//! Every other test runs with telemetry OFF, so without this one an exporter
//! that cannot reach its collector (an SDK upgrade that moved export onto a
//! thread with no tokio reactor, say) fails only as a logged transport error
//! while the whole suite stays green.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use opentelemetry::Context;
use opentelemetry_proto::tonic::collector::metrics::v1::metrics_service_server::{
    MetricsService, MetricsServiceServer,
};
use opentelemetry_proto::tonic::collector::metrics::v1::{
    ExportMetricsServiceRequest, ExportMetricsServiceResponse,
};
use opentelemetry_proto::tonic::collector::trace::v1::trace_service_server::{
    TraceService, TraceServiceServer,
};
use opentelemetry_proto::tonic::collector::trace::v1::{
    ExportTraceServiceRequest, ExportTraceServiceResponse,
};
use tokio::net::TcpListener;
use tokio_stream::wrappers::TcpListenerStream;
use tonic::{Request, Response, Status};

use storage_proxy::config::Config;
use storage_proxy::otel::{MetricsExt, Telemetry};
use storage_proxy::router::Class;

/// What the receiver has been sent: metric names and span names.
#[derive(Default, Clone)]
struct Received {
    metrics: Arc<Mutex<Vec<String>>>,
    spans: Arc<Mutex<Vec<String>>>,
}

#[tonic::async_trait]
impl MetricsService for Received {
    async fn export(
        &self,
        req: Request<ExportMetricsServiceRequest>,
    ) -> Result<Response<ExportMetricsServiceResponse>, Status> {
        let mut seen = self.metrics.lock().unwrap();
        for rm in req.into_inner().resource_metrics {
            for sm in rm.scope_metrics {
                seen.extend(sm.metrics.into_iter().map(|m| m.name));
            }
        }
        Ok(Response::new(ExportMetricsServiceResponse::default()))
    }
}

#[tonic::async_trait]
impl TraceService for Received {
    async fn export(
        &self,
        req: Request<ExportTraceServiceRequest>,
    ) -> Result<Response<ExportTraceServiceResponse>, Status> {
        let mut seen = self.spans.lock().unwrap();
        for rs in req.into_inner().resource_spans {
            for ss in rs.scope_spans {
                seen.extend(ss.spans.into_iter().map(|s| s.name));
            }
        }
        Ok(Response::new(ExportTraceServiceResponse::default()))
    }
}

// Multi-thread, like the binary's `#[tokio::main]`: `shutdown()` is called
// from a runtime worker, exactly as `server::run_until` does.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn metrics_and_spans_reach_the_otlp_receiver() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let received = Received::default();
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(MetricsServiceServer::new(received.clone()))
            .add_service(TraceServiceServer::new(received.clone()))
            .serve_with_incoming(TcpListenerStream::new(listener)),
    );

    let cfg = Config {
        listen: "127.0.0.1:0".parse().unwrap(),
        replicas: vec!["127.0.0.1:1".into()],
        affinity_ttl: Duration::from_secs(300),
        otel_endpoint: Some(format!("http://{addr}")),
        trace_sample_ratio: 1.0,
        server_tls: None,
        backend_tls: None,
    };
    let telemetry = Telemetry::from_config(&cfg, Arc::new(|| 3));

    let metrics = telemetry.metrics();
    assert!(metrics.is_some(), "metrics pipeline must build");
    metrics.routed(Class::Begin);
    let tracer = telemetry.tracer().expect("span pipeline must build");
    let mut span = tracer.start(Class::Begin, &Context::new());
    span.set_replica(0);
    span.finish(0);

    // Flush + shut down: the last metrics and the batched span export here.
    telemetry.shutdown();

    // The receiver handles the export on this runtime; give it a moment.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    loop {
        let m = received.metrics.lock().unwrap().clone();
        let s = received.spans.lock().unwrap().clone();
        if m.iter().any(|n| n == "w17.proxy.routed_total")
            && s.iter().any(|n| n == "storage-proxy begin")
        {
            break;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "OTLP receiver never got the export: metrics={m:?} spans={s:?}"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}
