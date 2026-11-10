package dpopgw_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/restgw/dpopgw"
)

// The binding lookup is a call to the auth backend. A request whose proof
// does not verify — a forged signature, a proof for another path, a missing
// nonce — MUST be refused before that call, so a flood of junk DPoP
// requests costs the gate its own checks, not one backend round trip each.
func TestGate_BindingIsLookedUpOnlyForAProofThatVerified(t *testing.T) {
	nonces, err := dpop.NewHMACNonces([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	k := dpKey(t)
	var calls atomic.Int32
	g, err := dpopgw.NewGate(dpVerifier(t, nonces), dpopgw.WithBinding(func(context.Context, string) (string, error) {
		calls.Add(1)
		return k.Thumbprint(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	nonce, _ := nonces.Issue(k.Thumbprint())

	forged := dpReq(t, k, http.MethodGet, "/v1/notes", "tok", nonce)
	p := forged.Header.Get("DPoP")
	forged.Header.Set("DPoP", p[:len(p)-4]+"AAAA")
	otherPath := dpReq(t, k, http.MethodGet, "/v1/other", "tok", nonce)
	otherPath.URL.Path = "/v1/notes"
	noNonce := dpReq(t, k, http.MethodGet, "/v1/notes", "tok", "")
	for name, r := range map[string]*http.Request{"forged signature": forged, "another path": otherPath, "no nonce": noNonce} {
		if _, err := g.Verify(r); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the binding was looked up %d times for proofs that did not verify", n)
	}

	good := dpReq(t, k, http.MethodGet, "/v1/notes", "tok", nonce)
	v, err := g.Verify(good)
	if err != nil || v.Thumbprint != k.Thumbprint() {
		t.Fatalf("a good bound request: %+v, %v", v, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a good request looked the binding up %d times, want 1", n)
	}

	// A good proof bound to another key is still a binding mismatch.
	other := dpKey(t)
	n2, _ := nonces.Issue(other.Thumbprint())
	if _, err := g.Verify(dpReq(t, other, http.MethodGet, "/v1/notes", "tok", n2)); !errors.Is(err, dpop.ErrBindingMismatch) {
		t.Fatalf("a proof by another key: %v, want ErrBindingMismatch", err)
	}
}
