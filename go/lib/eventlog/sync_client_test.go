package eventlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A sync audit record is finished inside the handler — before the response,
// so it is stored before the client hears back — but the middleware used to
// set the client (IP, user agent, referrer, UTM) and the request id only
// after the handler returned: on the finished record, which the batcher had
// already encoded. Every sync audit record was stored without them. The
// middleware now captures them before the handler runs and the record takes
// them when it begins.

func TestHTTPMiddleware_SyncAuditThroughConnKeepsTheClient(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_ADMIN: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_ADMIN)
	sync := *testMethod
	sync.Sync = true
	cc := s.Conn(&fakeConn{}, map[string]*Method{testMethod.FullName: &sync})
	mw := s.HTTPMiddleware(ratelimit.ParseTrustedProxies("10.0.0.0/8"), func(context.Context) string { return "rid-sync" })
	req := httptest.NewRequest("POST", "/admin/api/action/x", nil)
	req.RemoteAddr = "10.0.0.2:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("User-Agent", "sync-agent")
	mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := metadata.NewOutgoingContext(req.Context(), authMD("staff-1", false, "org-1"))
		_ = cc.Invoke(ctx, testMethod.FullName, sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_ADMIN)
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	m := decodeEnvelope(t, recs[0]).meta
	if m.GetClient().GetIp() != "198.51.100.7" || m.GetClient().GetUserAgent() != "sync-agent" || m.GetRequestId() != "rid-sync" {
		t.Errorf("sync audit record: ip=%q ua=%q request_id=%q, want 198.51.100.7, sync-agent, rid-sync",
			m.GetClient().GetIp(), m.GetClient().GetUserAgent(), m.GetRequestId())
	}
}

func TestHTTPMiddleware_SyncAuditFinishedInTheHandlerKeepsTheClient(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	sync := *testMethod
	sync.Sync = true
	mw := s.HTTPMiddleware(nil, func(context.Context) string { return "rid-rest" })
	req := httptest.NewRequest("POST", "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.9:4444"
	req.Header.Set("User-Agent", "sync-agent")
	mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := Begin(req.Context(), s, &sync)
		rec.SetRequest(sampleReq())
		rec.SetCallerOutgoing(metadata.NewOutgoingContext(req.Context(), authMD("u-1", false, "org-1")))
		rec.SetResult(nil, nil)
		rec.Finish(req.Context(), http.StatusOK) // the generated sync REST handler's order
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	m := decodeEnvelope(t, recs[0]).meta
	if m.GetClient().GetIp() != "203.0.113.9" || m.GetClient().GetUserAgent() != "sync-agent" || m.GetRequestId() != "rid-rest" {
		t.Errorf("sync audit record: ip=%q ua=%q request_id=%q, want 203.0.113.9, sync-agent, rid-rest",
			m.GetClient().GetIp(), m.GetClient().GetUserAgent(), m.GetRequestId())
	}
}

// When SyncWait runs out the record stays queued and the handler goes on;
// the middleware then wrote the client into a record the batcher reads on
// its own goroutine — a data race (this test fails under -race without the
// fix). Nothing touches a finished record now.
func TestHTTPMiddleware_LateSyncRecordIsNotWrittenAfterFinish(t *testing.T) {
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_ADMIN: {Audit: true}},
		func(cfg *RecorderConfig) {
			cfg.SyncWait = 50 * time.Millisecond
			cfg.Sink = func(w17pb.RequestApi) func(context.Context, *Batch) error {
				return func(context.Context, *Batch) error { time.Sleep(300 * time.Millisecond); return nil }
			}
		})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_ADMIN)
	// Fill the sender and its queue so the batcher blocks and the sync record waits past SyncWait.
	for i := 0; i < 7; i++ {
		rec := s.Start(testMethod)
		rec.SetRequest(sampleReq())
		rec.Finish(context.Background(), 200)
		time.Sleep(30 * time.Millisecond)
	}
	sync := *testMethod
	sync.Sync = true
	cc := s.Conn(&fakeConn{}, map[string]*Method{testMethod.FullName: &sync})
	mw := s.HTTPMiddleware(nil, func(context.Context) string { return "rid" })
	req := httptest.NewRequest("POST", "/admin/api/action/x", nil)
	req.Header.Set("User-Agent", "late-agent")
	mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = cc.Invoke(req.Context(), testMethod.FullName, sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
}
