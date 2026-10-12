package restgw

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// TestNextPageURL pins the link shape: origin-relative, the cursor set on
// the query, and nothing taken from X-Forwarded-Proto, TLS or Host.
func TestNextPageURL(t *testing.T) {
	for _, xfp := range []string{"", "https", "https, http", "ftp"} {
		r := httptest.NewRequest("GET", "http://api.example/v1/tasks?limit=10", nil)
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		r.TLS = &tls.ConnectionState{}
		if got, want := nextPageURL(r, "cur123"), "/v1/tasks?cursor=cur123&limit=10"; got != want {
			t.Errorf("XFP %q: nextPageURL = %q, want %q", xfp, got, want)
		}
	}
}
