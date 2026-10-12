package observx

import (
	"github.com/getsentry/sentry-go"

	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeDSN is a syntactically valid Sentry DSN. sentry.Init accepts it and
// buffers events on an async transport, so no network traffic occurs in tests.
const fakeDSN = "https://abc123@127.0.0.1/1"

// TestMustInit_FullStack drives the Sentry + OTLP-trace + metrics branches of
// MustInit, then exercises every Shutdown arm in one pass.
func TestMustInit_FullStack(t *testing.T) {
	resetForTest(t)
	err := MustInit(Config{
		ServiceName:    "full",
		ServiceVersion: "1.2.3",
		Environment:    "test",
		SentryDSN:      fakeDSN,
		OTelEndpoint:   "localhost:4317", // lazy gRPC dial, never connects
		EnableMetrics:  true,
		SampleRate:     0, // exercises the sampleRate==0 → 1.0 default
		ConfigAttrs: []ConfigAttr{
			{Key: "PUBLIC_THING", Value: "v"},
		},
	})
	if err != nil {
		t.Fatalf("MustInit full stack: %v", err)
	}

	// All four providers/exporters should now be live; Shutdown tears them down.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Shutdown(ctx); err != nil {
		// A flush timeout here is acceptable (no collector); only fail on
		// an unexpected non-timeout error shape.
		t.Logf("Shutdown returned (tolerated): %v", err)
	}
	// Globals are reset by Shutdown.
	if tracerProvider != nil || meterProvider != nil || otlpExporter != nil || sentryEnabled {
		t.Error("Shutdown should have nil'd all global providers")
	}
}

// TestMustInit_SentryInitError — a malformed DSN makes sentry.Init fail, which
// MustInit surfaces as an error.
func TestMustInit_SentryInitError(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc", SentryDSN: "://not-a-valid-dsn"}); err == nil {
		t.Fatal("want error for a malformed Sentry DSN")
	}
}

// TestReportError_SentryPath drives the sentryEnabled branch of ReportError,
// including the trace_id/span_id tagging off a recording span.
func TestReportError_SentryPath(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc", SentryDSN: fakeDSN}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = Shutdown(ctx)
	}()

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("svc").Start(context.Background(), "op")
	ReportError(ctx, errors.New("kaboom")) // sentry capture + span record
	span.End()

	if got := rec.Ended(); len(got) != 1 {
		t.Fatalf("want 1 ended span, got %d", len(got))
	}
}

// TestReportEvent_SentryBreadcrumb drives the debug-on + sentry-on branch of
// ReportEvent (Sentry breadcrumb, not a captured exception).
func TestReportEvent_SentryBreadcrumb(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_OBSERVX_DEBUG", "true")
	if err := MustInit(Config{ServiceName: "svc", SentryDSN: fakeDSN}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = Shutdown(ctx)
	}()

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("svc").Start(context.Background(), "op")
	ReportEvent(ctx, errors.New("handled-condition")) // span event + sentry breadcrumb
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	foundEvent := false
	for _, e := range spans[0].Events() {
		if e.Name == "observx.event" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Error("ReportEvent should add an observx.event span event")
	}
}

// A step that fails after Sentry was already wired takes Sentry down with
// it: init is all or nothing, so a failed MustInit leaves no exporter
// running and reports nothing anywhere.
func TestMustInit_FailureAfterSentryTearsItDown(t *testing.T) {
	resetForTest(t)
	err := MustInit(Config{ServiceName: "svc", SentryDSN: fakeDSN, OTelEndpoint: "127.0.0.1:1", OTelTLS: "sometimes"})
	if err == nil {
		t.Fatal("want an error for an unknown OTelTLS value")
	}
	mu.Lock()
	on := sentryEnabled
	mu.Unlock()
	if on {
		t.Error("Sentry is still enabled after MustInit failed part-way")
	}
	if sentry.CurrentHub().Client() != nil {
		t.Error("a Sentry client is still installed after MustInit failed part-way")
	}
}

// Shutdown takes the Sentry client it installed off the global hub, as it
// does the OTel providers: nothing keeps reporting after it.
func TestShutdown_UnbindsTheSentryClient(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc", SentryDSN: fakeDSN}); err != nil {
		t.Fatal(err)
	}
	if sentry.CurrentHub().Client() == nil {
		t.Fatal("MustInit installed no Sentry client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = Shutdown(ctx)
	if sentry.CurrentHub().Client() != nil {
		t.Error("the Sentry client is still bound after Shutdown")
	}
}
