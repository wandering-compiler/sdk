package restgw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// The org-scope header must be advertised BY DEFAULT, not left to an
// operator's CORS_HEADERS.
//
// Omitting it fails in the worst possible way: the preflight still answers
// 204, the browser then silently refuses to send the real request, and the
// caller sees "Failed to fetch" with no status — while nothing reaches the
// server, so no log records an attempt. That is how the console's members and
// invites screens were unreachable on 2026-09-11 while /auth/orgs, which
// needs no org header, worked normally.
func TestPreflightAdvertisesOrgScopeHeaderByDefault(t *testing.T) {
	h := CORSMiddleware(
		CORSConfig{AllowedOrigins: []string{"https://w17.app"}},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)
	req := httptest.NewRequest(http.MethodOptions, "/auth/orgs/members", nil)
	req.Header.Set("Origin", "https://w17.app")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Exact list membership, not strings.Contains: a substring check passes
	// for "w17-org-anything", which a browser would NOT accept as the header
	// it asked for. Break-proofing caught that — the first version of this
	// test went green against a deliberately corrupted value.
	got := rec.Header().Get("Access-Control-Allow-Headers")
	found := false
	for _, h := range strings.Split(got, ",") {
		if strings.EqualFold(strings.TrimSpace(h), principal.OrgScopeHeader) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("preflight does not advertise %q — every org-scoped call is unreachable from a browser\n  got: %q",
			principal.OrgScopeHeader, got)
	}
}
