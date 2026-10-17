package eventlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

type fakeConn struct {
	grpc.ClientConnInterface
	calls []string
	err   error
}

func (f *fakeConn) Invoke(_ context.Context, method string, _, reply any, _ ...grpc.CallOption) error {
	f.calls = append(f.calls, method)
	if r, ok := reply.(*descriptorpb.EnumValueDescriptorProto); ok {
		r.Number = proto.Int32(42)
	}
	return f.err
}

// An admin request: the auth lookup passes through, the page's backend call
// begins the record, the middleware finishes it with the status.
func TestConn_RecordsTheRequestsBackendCall(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_ADMIN: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_ADMIN)
	backend := &fakeConn{}
	cc := s.Conn(backend, map[string]*Method{"/acme.tasks.TaskService/Create": testMethod})
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := metadata.NewOutgoingContext(req.Context(), authMD("staff-1", false, "org-1"))
		_ = cc.Invoke(ctx, "/acme.auth.Auth/Lookup", &descriptorpb.FieldDescriptorProto{}, &descriptorpb.EnumValueDescriptorProto{})
		_ = cc.Invoke(ctx, "/acme.tasks.TaskService/Create", sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
		_ = cc.Invoke(ctx, "/acme.tasks.TaskService/Create", sampleReq(), &descriptorpb.EnumValueDescriptorProto{}) // a second call: not recorded again
		w.WriteHeader(http.StatusOK)
	})
	s.HTTPMiddleware(nil, nil)(handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/api/action/x", nil))
	backend.err = status.Error(codes.PermissionDenied, "no")
	s.HTTPMiddleware(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = cc.Invoke(req.Context(), "/acme.tasks.TaskService/Create", sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
		w.WriteHeader(http.StatusForbidden)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/api/action/x", nil))
	// A call outside any HTTP request passes through unrecorded.
	_ = cc.Invoke(context.Background(), "/acme.tasks.TaskService/Create", sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
	closeRecorder(t, r)

	recs := c.records(w17pb.RequestApi_REQUEST_API_ADMIN)
	if len(recs) != 2 || len(backend.calls) != 5 {
		t.Fatalf("%d records, %d backend calls", len(recs), len(backend.calls))
	}
	ok, refused := decodeEnvelope(t, recs[0]), decodeEnvelope(t, recs[1])
	if ok.meta.GetUserId() != "staff-1" || ok.meta.GetHttpStatus() != 200 || ok.req == nil || ok.resp == nil {
		t.Fatalf("first record %+v", ok.meta)
	}
	if refused.meta.GetGrpcCode() != int32(codes.PermissionDenied) || refused.meta.GetHttpStatus() != 403 || refused.resp != nil {
		t.Fatalf("refused record %+v", refused.meta)
	}
	var nilS *Surface
	if nilS.Conn(backend, nil) != grpc.ClientConnInterface(backend) {
		t.Fatal("a nil surface wrapped the conn")
	}
}

// A sync audit method: the backend call returns to the handler only once its
// record is stored — before the handler answers.
func TestConn_SyncAuditHoldsTheCall(t *testing.T) {
	gate := make(chan struct{})
	var stored atomic.Int32
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_ADMIN: {Audit: true}},
		func(cfg *RecorderConfig) {
			cfg.Sink = func(w17pb.RequestApi) func(context.Context, *Batch) error {
				return func(context.Context, *Batch) error { <-gate; stored.Add(1); return nil }
			}
		})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_ADMIN)
	sync := *testMethod
	sync.Sync = true
	cc := s.Conn(&fakeConn{}, map[string]*Method{"/acme.tasks.TaskService/Create": &sync})
	answered := make(chan int32, 1)
	go s.HTTPMiddleware(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = cc.Invoke(req.Context(), "/acme.tasks.TaskService/Create", sampleReq(), &descriptorpb.EnumValueDescriptorProto{})
		answered <- stored.Load()
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/api/action/x", nil))
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if n := <-answered; n != 1 {
		t.Fatalf("the handler answered with %d records stored", n)
	}
	closeRecorder(t, r)
	if stored.Load() != 1 {
		t.Fatalf("stored %d records (the middleware finished it again?)", stored.Load())
	}
}
