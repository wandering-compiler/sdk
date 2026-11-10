package eventlog

import (
	"net/http"
	"net/http/httptest"
	"testing"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A handler that panics after Begin is still recorded, as a 500: the
// middleware finishes the record while the panic unwinds through it (an
// outer recovery middleware answers the client), then lets the panic go on.
func TestHTTPMiddleware_HandlerPanicAfterBeginIsStillRecorded(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	mw := s.HTTPMiddleware(nil, nil)
	recovering := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, req)
		})
	}
	req := httptest.NewRequest("POST", "/api/tasks", nil)
	recovering(mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := Begin(req.Context(), s, testMethod)
		rec.SetRequest(sampleReq())
		panic("nil map in the handler")
	}))).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
	if n := len(c.records(w17pb.RequestApi_REQUEST_API_REST)); n != 1 {
		t.Fatalf("%d audit records of a request whose handler panicked after Begin, want 1 (fails=%d)", n, len(c.fails))
	}
}

// A second Begin in one request keeps the first record: both are finished
// with the request's status.
func TestHTTPMiddleware_SecondBeginKeepsTheFirstRecord(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	mw := s.HTTPMiddleware(nil, nil)
	second := *testMethod
	second.FullName = "/acme.tasks.TaskService/Second"
	req := httptest.NewRequest("POST", "/api/tasks", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		a := Begin(req.Context(), s, testMethod)
		a.SetRequest(sampleReq())
		b := Begin(req.Context(), s, &second)
		b.SetRequest(sampleReq())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 2 {
		t.Fatalf("%d records for two Begin calls, want 2 (fails=%d)", len(recs), len(c.fails))
	}
}
