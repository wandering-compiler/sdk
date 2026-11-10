package dpop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// An unbound (mint) proof has the same reuse window as a bound one: the
// byte-identical proof is accepted again while the window lasts — the window
// exists for a transport retry, and the client's gRPC retry of a sign-in
// replays the same metadata — and refused after it. What bounds a mint
// replay inside the window is the per-address limit in front of the mint
// endpoint (E35 deliberate limits), not the verifier.
func TestVerify_AMintProofHasTheBoundProofsReuseWindow(t *testing.T) {
	for _, bound := range []bool{false, true} {
		c := newClock()
		nonces := hmacNonces(t, c, time.Hour)
		v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
		k := mustKey(t)
		nonce, _ := nonces.Issue(k.Thumbprint())
		cl := claims(c, nonce)
		req := func(p string) dpop.Request { return get(p) }
		if bound {
			cl.ATH = dpop.AccessTokenHash("tok")
			req = func(p string) dpop.Request {
				r := get(p)
				r.AccessToken, r.Binding = "tok", k.Thumbprint()
				return r
			}
		}
		p := sign(t, k, cl)
		ctx := context.Background()
		if _, err := v.Verify(ctx, req(p)); err != nil {
			t.Fatalf("bound=%v first: %v", bound, err)
		}
		c.Advance(29 * time.Second)
		if _, err := v.Verify(ctx, req(p)); err != nil {
			t.Fatalf("bound=%v: the same proof inside the window = %v, want accepted", bound, err)
		}
		c.Advance(2 * time.Second)
		if _, err := v.Verify(ctx, req(p)); !errors.Is(err, dpop.ErrReplay) {
			t.Fatalf("bound=%v: after the window = %v, want ErrReplay", bound, err)
		}
	}
}
