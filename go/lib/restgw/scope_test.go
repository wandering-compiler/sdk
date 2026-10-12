package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// ScopeKey pins the metadata-key naming convention.
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

// WriteMissingScope writes 403 PERMISSION_DENIED with the code's catalog
// sentence. The scope's name is configuration and goes to the operator only.
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
	if strings.Contains(body, "tenant_id") {
		t.Errorf("body = %q, the scope name must not reach the client", body)
	}
	if !strings.Contains(body, "You do not have access to this.") {
		t.Errorf("body = %q, want the code's catalog sentence", body)
	}
}
