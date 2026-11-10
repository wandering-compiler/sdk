package eventlog

import (
	"context"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A record that never reached a batch (refused: the pipeline closed, no
// room) goes to the failure path, encoded on the caller's goroutine; it must
// not also be rendered to the telemetry backend there, as if it had been
// recorded.
func TestRecorder_ARefusedRecordIsNotRenderedToTelemetry(t *testing.T) {
	exp := &collectingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	observx.UseRequestLogger(lp)
	t.Cleanup(func() { observx.UseRequestLogger(nil); _ = lp.Shutdown(context.Background()) })

	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	closeRecorder(t, r)
	rec := s.Start(testMethod)
	rec.SetRequest(sampleReq())
	rec.Finish(context.Background(), 200)

	if len(c.fails) != 1 || len(c.fails[0].Records) != 1 {
		t.Fatalf("failures = %+v; want the refused record on the failure path with its encoding", c.fails)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.recs) != 0 {
		t.Fatalf("%d telemetry records for a record that was refused", len(exp.recs))
	}
}

// A marketing record set to drop costs nothing on the request path when
// there is no room: it goes to the failure path without being encoded.
func TestRecorder_ADroppedRecordIsNotEncoded(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	closeRecorder(t, r)
	drop := *testMethod
	drop.Drop = true
	rec := s.Start(&drop)
	rec.SetRequest(sampleReq())
	rec.SetCaller(authMD("u-1", true, "org-1"))
	rec.Finish(context.Background(), 200)
	if len(c.fails) != 1 || c.fails[0].Count != 1 || len(c.fails[0].Records) != 0 {
		t.Fatalf("failures = %+v; want one dropped record with no encoding", c.fails)
	}
}
