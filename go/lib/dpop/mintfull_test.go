package dpop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// failingStore is a JTIStore whose backend is down.
type failingStore struct{}

func (failingStore) Record(context.Context, string, dpop.JTIRecord, time.Duration) (dpop.JTIRecord, bool, error) {
	return dpop.JTIRecord{}, false, errors.New("backend down")
}

// boundRequest is a valid bound GET /r for k and token "tok".
func boundRequest(t *testing.T, c *clock, k *dpop.Key, jti string) dpop.Request {
	t.Helper()
	cl := claims(c, "")
	cl.JTI = jti
	cl.ATH = dpop.AccessTokenHash("tok")
	req := get(sign(t, k, cl))
	req.AccessToken, req.Binding = "tok", k.Thumbprint()
	return req
}

// With MintFullAcceptUnrecorded an unbound proof meeting a full mint store is
// accepted unrecorded and reported; a recorded proof is still a replay, and
// bound proofs and failing stores still fail closed.
func TestVerify_MintFullAcceptUnrecorded(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	mint, bound := dpop.NewMemoryJTIStore(1), dpop.NewMemoryJTIStore(1)
	v := newVerifier(t, nil, func(cfg *dpop.Config) {
		cfg.Now, cfg.ReuseWindow = c.Now, -1
		cfg.JTIs, cfg.MintJTIs = bound, mint
		cfg.MintFull = dpop.MintFullAcceptUnrecorded
	})

	first := get(sign(t, mustKey(t), claims(c, "")))
	r, err := v.VerifyResult(ctx, first)
	if err != nil || r.MintUnrecorded || r.Thumbprint == "" {
		t.Fatalf("first unbound proof = (%+v, %v), want recorded", r, err)
	}
	k := mustKey(t)
	r, err = v.VerifyResult(ctx, get(sign(t, k, claims(c, ""))))
	if err != nil || !r.MintUnrecorded || r.Thumbprint != k.Thumbprint() {
		t.Fatalf("unbound proof on a full mint store = (%+v, %v), want accepted unrecorded", r, err)
	}
	if mint.Len() != 1 {
		t.Fatalf("mint store holds %d entries, want 1: the unrecorded proof was recorded", mint.Len())
	}
	if thumb, err := v.Verify(ctx, get(sign(t, mustKey(t), claims(c, "")))); err != nil || thumb == "" {
		t.Fatalf("Verify on a full mint store = (%q, %v), want the thumbprint", thumb, err)
	}
	if _, err := v.VerifyResult(ctx, first); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("replay of a recorded unbound proof = %v, want ErrReplay", err)
	}

	// Bound proofs never: a full bound store still refuses.
	bk := mustKey(t)
	if _, err := v.VerifyResult(ctx, boundRequest(t, c, bk, "b1")); err != nil {
		t.Fatalf("first bound proof: %v", err)
	}
	r, err = v.VerifyResult(ctx, boundRequest(t, c, bk, "b2"))
	if !errors.Is(err, dpop.ErrUnavailable) || !errors.Is(err, dpop.ErrJTIStoreFull) || r.Thumbprint != "" {
		t.Fatalf("bound proof on a full bound store = (%+v, %v), want ErrUnavailable", r, err)
	}

	// A failing mint store is not a full one: refused.
	down := newVerifier(t, nil, func(cfg *dpop.Config) {
		cfg.Now = c.Now
		cfg.JTIs, cfg.MintJTIs = dpop.NewMemoryJTIStore(0), failingStore{}
		cfg.MintFull = dpop.MintFullAcceptUnrecorded
	})
	if r, err := down.VerifyResult(ctx, get(sign(t, mustKey(t), claims(c, "")))); !errors.Is(err, dpop.ErrUnavailable) || errors.Is(err, dpop.ErrJTIStoreFull) || r.Thumbprint != "" {
		t.Fatalf("unbound proof on a failing mint store = (%+v, %v), want ErrUnavailable", r, err)
	}
}

// The default policy refuses an unbound proof the full mint store cannot
// record, and never reports one as unrecorded.
func TestVerify_MintFullRefuseIsTheDefault(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	v := newVerifier(t, nil, func(cfg *dpop.Config) {
		cfg.Now = c.Now
		cfg.JTIs, cfg.MintJTIs = dpop.NewMemoryJTIStore(0), dpop.NewMemoryJTIStore(1)
	})
	if r, err := v.VerifyResult(ctx, get(sign(t, mustKey(t), claims(c, "")))); err != nil || r.MintUnrecorded {
		t.Fatalf("first unbound proof = (%+v, %v)", r, err)
	}
	r, err := v.VerifyResult(ctx, get(sign(t, mustKey(t), claims(c, ""))))
	if !errors.Is(err, dpop.ErrUnavailable) || !errors.Is(err, dpop.ErrJTIStoreFull) || r != (dpop.Result{}) {
		t.Fatalf("unbound proof on a full mint store = (%+v, %v), want ErrUnavailable", r, err)
	}
	if _, err := dpop.NewVerifier(dpop.Config{Origin: testOrigin, MintFull: dpop.MintFullPolicy(7)}); err == nil {
		t.Error("an unknown MintFull policy was accepted")
	}
}
