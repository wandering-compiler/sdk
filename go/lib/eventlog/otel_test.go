package eventlog

import (
	"context"
	"strings"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

type collectingExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (e *collectingExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rs {
		e.recs = append(e.recs, r.Clone())
	}
	return nil
}
func (e *collectingExporter) Shutdown(context.Context) error   { return nil }
func (e *collectingExporter) ForceFlush(context.Context) error { return nil }

// A sampled record reaches the telemetry backend as a log record joined to
// its span by trace and span id, its body the record as the stream carries
// it — without the sensitive field.
func TestTelemetrySink_EmitsTheRecordJoinedToItsSpan(t *testing.T) {
	exp := &collectingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	observx.UseRequestLogger(lp)
	t.Cleanup(func() { observx.UseRequestLogger(nil); _ = lp.Shutdown(context.Background()) })

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "request")
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(testMethod)
	rec.SetRequest(sampleReq())
	rec.SetResult(sampleResp(), nil)
	rec.Finish(ctx, 201)
	span.End()
	closeRecorder(t, r)

	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.recs) != 1 {
		t.Fatalf("%d log records", len(exp.recs))
	}
	lr := exp.recs[0]
	if lr.TraceID() != span.SpanContext().TraceID() || lr.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("log record not joined to its span: %s/%s", lr.TraceID(), lr.SpanID())
	}
	body := lr.Body().AsString()
	if strings.Contains(body, "DECOY") || !strings.Contains(body, `"name":"Write the report"`) ||
		!strings.Contains(body, testMethod.FullName) || !strings.Contains(body, `"number":42`) {
		t.Fatalf("body %s", body)
	}
	if lr.EventName() != "w17.request" {
		t.Fatalf("event name %q", lr.EventName())
	}
}

// Without a sink, nothing is rendered: the record does not even ask.
func TestTelemetrySink_OffWithoutASink(t *testing.T) {
	observx.UseRequestLogger(nil)
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(testMethod)
	rec.Finish(context.Background(), 200)
	if rec.otel {
		t.Fatal("sampled for a sink that is not there")
	}
	closeRecorder(t, r)
}
