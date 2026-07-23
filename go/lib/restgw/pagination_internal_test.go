package restgw

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNextPageURL pins the scheme resolution + cursor injection: a
// comma-separated X-Forwarded-Proto takes the first token, an invalid
// proto falls back to http, TLS implies https, and the cursor is set on
// the query.
func TestNextPageURL(t *testing.T) {
	mk := func(xfp string) string {
		r := httptest.NewRequest("GET", "http://api.example/v1/tasks?limit=10", nil)
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		return nextPageURL(r, "cur123")
	}
	// comma-separated list → first token (https), and cursor injected.
	got := mk("https, http")
	if !strings.HasPrefix(got, "https://") || !strings.Contains(got, "cursor=cur123") {
		t.Errorf("comma XFP = %q, want https + cursor", got)
	}
	// single valid proto.
	if !strings.HasPrefix(mk("https"), "https://") {
		t.Errorf("single https XFP not honored")
	}
	// invalid proto → http fallback.
	if !strings.HasPrefix(mk("ftp"), "http://") {
		t.Errorf("invalid XFP must fall back to http")
	}
	// no header, no TLS → http.
	if !strings.HasPrefix(mk(""), "http://") {
		t.Errorf("no XFP/TLS must be http")
	}
}
