package observx

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// resetForTest tears down the global state so tests don't
// pollute each other. Mirrors what Shutdown does, plus
// resets the cfg.
func resetForTest(t *testing.T) {
	t.Helper()
	mu.Lock()
	tracerProvider = nil
	otlpExporter = nil
	sentryEnabled = false
	debugEvents = false
	cfg = Config{}
	mu.Unlock()
	otel.SetTracerProvider(sdktrace.NewTracerProvider()) // reset to a clean noop-equivalent
}

func TestMustInit_ServiceNameRequired(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{}); err == nil {
		t.Error("empty ServiceName should fail")
	}
}

func TestMustInit_NoExporters_OK(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "test-service"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	// No Sentry, no OTel exporter — neither set; tracer
	// stays at SDK default. Should still work end-to-end.
	defer Shutdown(context.Background())
}

func TestReportError_NilNoOp(t *testing.T) {
	resetForTest(t)
	_ = MustInit(Config{ServiceName: "test"})
	defer Shutdown(context.Background())
	// Capture log to confirm no output for nil err.
	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)
	ReportError(context.Background(), nil)
	if buf.Len() != 0 {
		t.Errorf("nil err should be no-op; got log: %q", buf.String())
	}
}

func TestReportError_StderrFallback(t *testing.T) {
	resetForTest(t)
	_ = MustInit(Config{ServiceName: "test"}) // no Sentry, no OTel
	defer Shutdown(context.Background())

	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)

	ReportError(context.Background(), errors.New("boom"))
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("stderr fallback should log err: %q", buf.String())
	}
}

func TestReportError_AttachesToActiveOTelSpan(t *testing.T) {
	resetForTest(t)
	// Set up an in-memory span recorder so we can assert
	// the span got the error attached.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	defer tp.Shutdown(context.Background())

	cfg = Config{ServiceName: "test"} // bypass MustInit since we set TP manually

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	ReportError(ctx, errors.New("captured"))
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	events := spans[0].Events()
	foundErr := false
	for _, e := range events {
		if e.Name == "exception" {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("span should carry exception event from RecordError; events: %+v", events)
	}
}

// T-grpcerr-1: ReportEvent is the non-exception channel for
// handled, user-correctable conditions. It is gated behind
// W17_OBSERVX_DEBUG and is a no-op when the knob is off.
func TestReportEvent_OffByDefault_NoOp(t *testing.T) {
	resetForTest(t)
	_ = MustInit(Config{ServiceName: "test"}) // W17_OBSERVX_DEBUG unset → off
	defer Shutdown(context.Background())

	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)

	ReportEvent(context.Background(), errors.New("mapped constraint"))
	if buf.Len() != 0 {
		t.Errorf("debug events off by default should be a no-op; got log: %q", buf.String())
	}
}

// With the debug knob on, ReportEvent surfaces via the log
// fallback (no Sentry/OTel configured) — and crucially uses a
// `debug:` prefix, NOT the exception-level `error:` prefix.
func TestReportEvent_DebugKnob_StderrFallback(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_OBSERVX_DEBUG", "1")
	_ = MustInit(Config{ServiceName: "test"})
	defer Shutdown(context.Background())

	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)

	ReportEvent(context.Background(), errors.New("dup email"))
	out := buf.String()
	if !strings.Contains(out, "dup email") {
		t.Errorf("debug fallback should log the event: %q", out)
	}
	if strings.Contains(out, "error:") {
		t.Errorf("debug event must not use the exception-level prefix: %q", out)
	}
}

// The core distinction from ReportError: ReportEvent adds an
// informational span EVENT but must NOT mark the span failed
// (no SetStatus(Error)) nor record an exception event.
func TestReportEvent_SpanEvent_NoErrorStatus(t *testing.T) {
	resetForTest(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	defer tp.Shutdown(context.Background())

	cfg = Config{ServiceName: "test"} // bypass MustInit since we set TP manually
	debugEvents = true                // enable the channel directly

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	ReportEvent(ctx, errors.New("mapped constraint"))
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	// Span must NOT be marked failed — the whole point of the
	// non-exception channel vs ReportError.
	if spans[0].Status().Code == codes.Error {
		t.Errorf("ReportEvent must not SetStatus(Error); status=%+v", spans[0].Status())
	}
	foundEvent := false
	for _, e := range spans[0].Events() {
		if e.Name == "exception" {
			t.Errorf("ReportEvent must not record an exception event; events: %+v", spans[0].Events())
		}
		if e.Name == "observx.event" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Errorf("ReportEvent should add an informational span event; events: %+v", spans[0].Events())
	}
}

func TestTracer_ReturnsNonNil(t *testing.T) {
	resetForTest(t)
	_ = MustInit(Config{ServiceName: "test"})
	defer Shutdown(context.Background())
	if Tracer() == nil {
		t.Error("Tracer() should never return nil")
	}
}

// REV-033 Cat 5 sweep, F9 — OTEL_SERVICE_NAME overrides
// Config.ServiceName.
func TestMustInit_OTelServiceNameOverride(t *testing.T) {
	resetForTest(t)
	t.Setenv("OTEL_SERVICE_NAME", "tagged-canary")
	if err := MustInit(Config{ServiceName: "default-name"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer Shutdown(context.Background())
	if cfg.ServiceName != "tagged-canary" {
		t.Errorf("ServiceName = %q, want OTEL_SERVICE_NAME override", cfg.ServiceName)
	}
}

// REV-033 Cat 5 sweep, F9 — empty OTEL_SERVICE_NAME keeps
// the Config value (no spurious blanks).
func TestMustInit_OTelServiceNameEmptyKeepsConfig(t *testing.T) {
	resetForTest(t)
	t.Setenv("OTEL_SERVICE_NAME", "")
	if err := MustInit(Config{ServiceName: "kept-name"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer Shutdown(context.Background())
	if cfg.ServiceName != "kept-name" {
		t.Errorf("ServiceName = %q, want kept-name", cfg.ServiceName)
	}
}

// REV-033 Cat 5 sweep, F3 — OTEL_EXPORTER_OTLP_ENDPOINT
// fallback when Config.OTelEndpoint is empty.
func TestMustInit_OTelEndpointFallback(t *testing.T) {
	resetForTest(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "fallback:4317")
	// No real exporter dial — pass invalid endpoint inside
	// the Init flow, which will accept the address but fail
	// to connect lazily. We assert cfg.OTelEndpoint got
	// populated, not that the exporter dialed.
	defer func() {
		// init may fail at exporter dial; that's OK for the
		// fallback assertion. We check cfg.OTelEndpoint
		// after the lock-protected assignment.
		_ = recover()
	}()
	_ = MustInit(Config{ServiceName: "test", OTelEndpoint: ""})
	defer Shutdown(context.Background())
	if cfg.OTelEndpoint != "fallback:4317" {
		t.Errorf("OTelEndpoint = %q, want fallback:4317 from OTEL_EXPORTER_OTLP_ENDPOINT", cfg.OTelEndpoint)
	}
}

// REV-033 Cat 5 sweep, F3 — Config.OTelEndpoint wins over
// the SDK env (ops set both → branded W17 path takes
// precedence; predictable for ops who set both deliberately).
func TestMustInit_W17OTelEndpointWinsOverFallback(t *testing.T) {
	resetForTest(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "fallback:4317")
	defer func() { _ = recover() }()
	_ = MustInit(Config{ServiceName: "test", OTelEndpoint: "branded:4317"})
	defer Shutdown(context.Background())
	if cfg.OTelEndpoint != "branded:4317" {
		t.Errorf("OTelEndpoint = %q, want branded:4317 (W17 wins over SDK env)", cfg.OTelEndpoint)
	}
}

func TestShutdown_NilSafe(t *testing.T) {
	resetForTest(t)
	// Shutdown without prior Init should not panic.
	if err := Shutdown(context.Background()); err != nil {
		t.Errorf("nil-state shutdown should be no-op; got %v", err)
	}
}
