package eventlog

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The record ceiling bounded only the request and the response; the meta —
// the client's Referer, User-Agent and UTM parameters — went in verbatim.
// An unauthenticated request with a 100 KB Referer was stored at 100 KB past
// the 64 KiB ceiling, and one with a Referer near a megabyte (Go accepts
// headers up to 1 MiB) no longer fitted a batch: the audit record went to the
// failure path with ErrTooLarge, a way to make one's own request unaudited.
// Each client string is now cut to its own bound, and the cut is named.
func TestRecorder_ClientMetaIsBounded(t *testing.T) {
	for _, n := range []int{100 << 10, 1046000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true, Marketing: true}})
			s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
			mw := s.HTTPMiddleware(nil, nil)
			q := url.Values{}
			for i := 0; i < 100; i++ {
				q.Set("utm_k"+strconv.Itoa(i), strings.Repeat("v", 1000))
			}
			req := httptest.NewRequest("GET", "/api/tasks?"+q.Encode(), nil)
			req.Header.Set("Referer", strings.Repeat("r", n))
			req.Header.Set("User-Agent", strings.Repeat("a", n))
			mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				Begin(req.Context(), s, testMethod).SetRequest(sampleReq())
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(httptest.NewRecorder(), req)
			closeRecorder(t, r)
			c.mu.Lock()
			for _, f := range c.fails {
				if errors.Is(f.Err, ErrTooLarge) {
					t.Errorf("the record was lost to ErrTooLarge: %v", f.Err)
				}
			}
			c.mu.Unlock()
			recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
			if len(recs) != 1 {
				t.Fatalf("%d records stored, want 1", len(recs))
			}
			if len(recs[0]) > DefaultRecordCeiling {
				t.Errorf("record of %d bytes, over the %d ceiling", len(recs[0]), DefaultRecordCeiling)
			}
			m := decodeEnvelope(t, recs[0]).meta
			cl := m.GetClient()
			if len(cl.GetReferrer()) != MaxClientField || len(cl.GetUserAgent()) != MaxClientField {
				t.Errorf("referrer %d bytes, user agent %d bytes, want each cut to %d", len(cl.GetReferrer()), len(cl.GetUserAgent()), MaxClientField)
			}
			if len(cl.GetUtm()) != MaxUTMParams {
				t.Errorf("%d UTM parameters kept, want %d", len(cl.GetUtm()), MaxUTMParams)
			}
			for k, v := range cl.GetUtm() {
				if len(k) > MaxUTMField || len(v) > MaxUTMField {
					t.Errorf("UTM %d-byte key / %d-byte value kept, want at most %d each", len(k), len(v), MaxUTMField)
				}
			}
			for _, p := range []string{"client.referrer", "client.user_agent", "client.utm"} {
				if !slices.Contains(m.GetTruncatedPaths(), p) {
					t.Errorf("truncated paths %v do not name %s", m.GetTruncatedPaths(), p)
				}
			}
			if !m.GetTruncated() {
				t.Error("record not marked truncated")
			}
		})
	}
}

// A client string that is not valid UTF-8 cannot be a proto string: the
// whole record failed to encode. It is stored with the bad bytes replaced.
func TestRecorder_InvalidUTF8ClientIsStored(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	mw := s.HTTPMiddleware(nil, nil)
	req := httptest.NewRequest("GET", "/api/tasks", nil)
	req.Header.Set("Referer", "https://a.example/\xff")
	mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		Begin(req.Context(), s, testMethod).SetRequest(sampleReq())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records stored, want 1", len(recs))
	}
	if got := decodeEnvelope(t, recs[0]).meta.GetClient().GetReferrer(); got != "https://a.example/\uFFFD" {
		t.Errorf("referrer %q", got)
	}
}
