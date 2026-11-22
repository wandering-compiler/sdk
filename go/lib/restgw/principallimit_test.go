package restgw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// authResp marshals the fields an auth response leads with: permission ids
// (1, packed), user_id (2), a scope map entry (3).
func authResp(user string) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte{1, 2, 3})
	if user != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, user)
	}
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte{0x0a, 0x01, 'k'})
	return b
}

func TestPrincipalOf(t *testing.T) {
	if got := PrincipalOf(authResp("u-1")); got != "u-1" {
		t.Errorf("PrincipalOf = %q, want u-1", got)
	}
	for _, b := range [][]byte{nil, authResp(""), {0xff}} {
		if got := PrincipalOf(b); got != "" {
			t.Errorf("PrincipalOf(%x) = %q, want empty", b, got)
		}
	}
}

func TestAdmitPrincipal_RefusesOverBudgetWithItsWait(t *testing.T) {
	l := ratelimit.NewPrincipal("RG1", "REST", 60, 2)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	for i := 0; i < 2; i++ {
		if !AdmitPrincipal(l, httptest.NewRecorder(), r, authResp("u-1"), 1) {
			t.Fatalf("call %d refused within the burst", i)
		}
	}
	w := httptest.NewRecorder()
	if AdmitPrincipal(l, w, r, authResp("u-1"), 1) {
		t.Fatal("over budget admitted")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("refusal = %d Retry-After %q, want 429 and 1", w.Code, w.Header().Get("Retry-After"))
	}
	if !AdmitPrincipal(l, httptest.NewRecorder(), r, authResp("u-2"), 2) {
		t.Fatal("another principal refused")
	}
	if !AdmitPrincipal(l, httptest.NewRecorder(), r, authResp("u-1"), 0) {
		t.Fatal("an exempt method refused")
	}
	if !AdmitPrincipal(nil, httptest.NewRecorder(), r, authResp("u-1"), 5) {
		t.Fatal("limit off refused")
	}
}
