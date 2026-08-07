package restgw

import (
	"context"
	"testing"
)

// TestMatchOrigin pins CORS origin matching: empty origin → "", wildcard
// reflects the origin when credentials are on but returns "*" otherwise,
// an exact allowlist hit returns the origin, and a miss returns "".
func TestMatchOrigin(t *testing.T) {
	if matchOrigin("", []string{"*"}, false) != "" {
		t.Error("empty origin must be empty")
	}
	if got := matchOrigin("https://a.com", []string{"*"}, true); got != "https://a.com" {
		t.Errorf("wildcard+credentials = %q, want reflected origin", got)
	}
	if got := matchOrigin("https://a.com", []string{"*"}, false); got != "*" {
		t.Errorf("wildcard no-credentials = %q, want *", got)
	}
	if got := matchOrigin("https://a.com", []string{"https://a.com"}, false); got != "https://a.com" {
		t.Errorf("exact match = %q", got)
	}
	if got := matchOrigin("https://a.com", []string{"https://b.com"}, false); got != "" {
		t.Errorf("miss = %q, want empty", got)
	}
}

// TestRequestIDFromContext_Nil pins the nil-context guard.
func TestRequestIDFromContext_Nil(t *testing.T) {
	if RequestIDFromContext(context.TODO()) != "" {
		t.Error("ctx without request id must yield empty")
	}
	//lint:ignore SA1012 intentionally testing the nil-ctx guard
	if RequestIDFromContext(nil) != "" { //nolint:staticcheck
		t.Error("nil ctx must yield empty")
	}
}
