package eventlog

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

func TestHTTPMiddleware_FinishesTheRecordTheHandlerBegan(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true, Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := Begin(req.Context(), s, testMethod)
		rec.SetRequest(sampleReq())
		rec.SetResult(sampleResp(), nil)
		rec.SetCaller(authMD("u-1", true, "org-9"))
		if _, ok := w.(http.Flusher); !ok {
			t.Error("the wrapped writer lost Flush")
		}
		w.WriteHeader(http.StatusCreated)
	})
	unrecorded := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mw := s.HTTPMiddleware(ratelimit.ParseTrustedProxies("10.0.0.0/8"), func(context.Context) string { return "rid-1" })

	req := httptest.NewRequest("POST", "/api/tasks?utm_source=news&utm_campaign=fall&page=2", nil)
	req.RemoteAddr = "10.0.0.2:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Referer", "https://example.com/")
	mw(handler).ServeHTTP(httptest.NewRecorder(), req)
	mw(unrecorded).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	closeRecorder(t, r)

	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records (a route without Begin must record nothing)", len(recs))
	}
	m := decodeEnvelope(t, recs[0]).meta
	cl := m.GetClient()
	if m.GetHttpStatus() != 201 || cl.GetIp() != "198.51.100.7" || cl.GetUserAgent() != "test-agent" ||
		cl.GetReferrer() != "https://example.com/" || cl.GetUtm()["source"] != "news" || cl.GetUtm()["campaign"] != "fall" ||
		len(cl.GetUtm()) != 2 || m.GetRequestId() != "rid-1" || m.GetUserId() != "u-1" {
		t.Fatalf("meta %v", m)
	}
}

// A route that takes its credential in the URL records no referrer: the
// page that called it was opened from a capability link, and its address
// carries the same credential. The rest of the client stays.
func TestHTTPMiddleware_URLCredentialRouteRecordsNoReferrer(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	link := &Method{FullName: "/acme.docs.DocService/GetPublic", Route: "/public/docs/{token}", RequestTag: reqTag, URLCredential: true}
	mw := s.HTTPMiddleware(nil, nil)
	for _, m := range []*Method{link, testMethod} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			Begin(req.Context(), s, m).SetRequest(sampleReq())
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest("GET", "/public/docs/tok-123", nil)
		req.Header.Set("User-Agent", "test-agent")
		req.Header.Set("Referer", "https://app.example.com/invoice/tok-123")
		mw(handler).ServeHTTP(httptest.NewRecorder(), req)
	}
	closeRecorder(t, r)

	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	got := map[string]*w17pb.RequestClient{}
	for _, raw := range recs {
		m := decodeEnvelope(t, raw).meta
		got[m.GetMethod()] = m.GetClient()
	}
	if cl := got[link.FullName]; cl.GetReferrer() != "" || cl.GetUserAgent() != "test-agent" {
		t.Errorf("credential route client %v: want no referrer, the rest kept", cl)
	}
	// The decoy: the same request on an ordinary route keeps its referrer.
	if cl := got[testMethod.FullName]; cl.GetReferrer() != "https://app.example.com/invoice/tok-123" {
		t.Errorf("ordinary route client %v: want the referrer", cl)
	}
}

func TestHTTPMiddleware_OffSurfaceAndNoSlot(t *testing.T) {
	var s *Surface
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if got := s.HTTPMiddleware(nil, nil)(h); got == nil {
		t.Fatal("nil handler")
	}
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	if Begin(context.Background(), r.Surface(w17pb.RequestApi_REQUEST_API_REST), testMethod) != nil {
		t.Fatal("Begin without the middleware's slot returned a record")
	}
	closeRecorder(t, r)
}

// A WebSocket upgrade hijacks the connection through the wrapper.
func TestHTTPMiddleware_KeepsHijack(t *testing.T) {
	r, _ := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	srv := httptest.NewServer(s.HTTPMiddleware(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack through the wrapper: %v", err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
		_ = rw.Flush()
		_ = conn.Close()
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err == nil {
		_, _ = bufio.NewReader(resp.Body).ReadString('\n')
		_ = resp.Body.Close()
	}
	closeRecorder(t, r)
}
