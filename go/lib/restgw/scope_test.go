package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-147 — ScopeKey pins the metadata-key naming convention.
// Both gateway emit + storage emit call this function so the
// two sides agree on the wire format without literal-string
// duplication.
func TestScopeKey(t *testing.T) {
	cases := map[string]string{
		"tenant_id":    "x-w17-scope-tenant_id",
		"workspace_id": "x-w17-scope-workspace_id",
		// Underscore preserved (no snake → kebab conversion).
		"user_org_id": "x-w17-scope-user_org_id",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			if got := restgw.ScopeKey(name); got != want {
				t.Errorf("ScopeKey(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

// REV-147 — WriteMissingScope writes 403 PERMISSION_DENIED
// with the canonical message format. Generated storage
// handlers call this when a required scope is absent from
// the incoming gRPC metadata.
func TestWriteMissingScope(t *testing.T) {
	rec := httptest.NewRecorder()
	restgw.WriteMissingScope(rec, "tenant_id")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PERMISSION_DENIED") {
		t.Errorf("body = %q, want PERMISSION_DENIED", body)
	}
	if !strings.Contains(body, "missing required scope: tenant_id") {
		t.Errorf("body = %q, want canonical missing-scope message", body)
	}
}
