package dpope2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/dpope2e"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/stress"
)

// loadGateway verifies every proof with lib/dpop's verifier (nonces on): a
// mint at /signin binds a token to the proof's key; /me accepts that token
// only with a proof by the same key.
type loadGateway struct {
	*httptest.Server
	v       *dpop.Verifier
	mu      sync.Mutex
	tokens  map[string]string
	refused atomic.Int32
	nonced  atomic.Int32
}

func newLoadGateway(t *testing.T) *loadGateway {
	t.Helper()
	g := &loadGateway{tokens: map[string]string{}}
	nonces, err := dpop.NewHMACNonces([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	if g.v, err = dpop.NewVerifier(dpop.Config{Origin: g.URL, Nonces: nonces}); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *loadGateway) serve(w http.ResponseWriter, r *http.Request) {
	tok, isDPoP := dpop.AccessToken(r.Header.Get("Authorization"))
	mint := r.URL.Path == "/signin"
	g.mu.Lock()
	binding := g.tokens[tok]
	g.mu.Unlock()
	if mint != (r.Header.Get("Authorization") == "") || (!mint && (!isDPoP || binding == "")) {
		g.refused.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	jkt, err := g.v.Verify(r.Context(), dpop.Request{
		Method: r.Method, Path: r.URL.EscapedPath(), Proofs: r.Header.Values(dpop.HeaderProof),
		AccessToken: tok, Binding: binding,
	})
	if err != nil {
		dpop.SetChallenge(w.Header(), err)
		if _, ok := err.(*dpop.NonceError); ok {
			g.nonced.Add(1)
		} else {
			g.refused.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if n, err := g.v.Nonce(jkt); err == nil {
		w.Header().Set(dpop.HeaderNonce, n)
	}
	w.Header().Set("Content-Type", "application/json")
	if mint {
		g.mu.Lock()
		tok := fmt.Sprintf("tok-%d", len(g.tokens)+1)
		g.tokens[tok] = jkt
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jkt": jkt})
}

// The load signs with the key that minted the setup's session — through the
// nonce challenge, once per key — and every request succeeds.
func TestRun_DPoPLoadSignsWithTheSetupKey(t *testing.T) {
	g := newLoadGateway(t)
	setup := []runner.Step{{
		Label:    "signin",
		Endpoint: runner.Endpoint{Ref: "auth.SignIn", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/signin", Mint: true},
		Input:    map[string]any{"email": "a"},
		Expect:   map[string]any{"token": map[string]any{"capture": "auth.token"}},
	}}
	load := []stress.Op{{Endpoint: runner.Endpoint{Ref: "me.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/me", AuthRequired: true}}}
	callers := map[string]runner.Caller{"rest": runner.NewRESTCaller(g.URL, nil).WithSigner(dpope2e.New())}
	lc := stress.NewLoadCaller(g.URL, 2, 5*time.Second).WithSigner(dpope2e.New())

	rep, err := stress.Run(context.Background(), "dpop", stress.Plan{Concurrency: 2, TotalRequests: 20, Mode: stress.ModePool}, setup, load, nil, callers, lc)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Success != rep.Requests || rep.Requests == 0 {
		t.Errorf("load: %d of %d succeeded (4xx %d), want all", rep.Success, rep.Requests, rep.HTTP4xx)
	}
	if n := g.refused.Load(); n != 0 {
		t.Errorf("the gateway refused %d request(s)", n)
	}
	// One nonce challenge for the setup's transport, at most one per load
	// worker racing the first nonce — never one per request.
	if n := g.nonced.Load(); n < 1 || n > 3 {
		t.Errorf("%d nonce challenges, want the first proof's (and the racing workers') only", n)
	}
}

// A mint op under load carries an unbound proof and no token.
func TestFireAs_MintCarriesAnUnboundProof(t *testing.T) {
	g := newLoadGateway(t)
	lc := stress.NewLoadCaller(g.URL, 1, 5*time.Second).WithSigner(dpope2e.New())
	ep := runner.Endpoint{Ref: "auth.SignIn", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/signin", Mint: true}
	code, err := lc.FireAs(nil, ep, map[string]any{"email": "a"}, "", nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("mint: status %d, err %v", code, err)
	}
	if n := g.refused.Load(); n != 0 {
		t.Errorf("the gateway refused %d request(s)", n)
	}
}

// Without WithSigner the load sends the bearer and no proof, as before.
func TestFire_OffModeSendsTheBearer(t *testing.T) {
	var auth, proof string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, proof = r.Header.Get("Authorization"), r.Header.Get(dpop.HeaderProof)
	}))
	defer srv.Close()
	lc := stress.NewLoadCaller(srv.URL, 1, 5*time.Second)
	ep := runner.Endpoint{Ref: "me.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/me", AuthRequired: true, Mint: true}
	if _, err := lc.Fire(ep, nil, "tok", nil); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || proof != "" {
		t.Errorf("Authorization %q, DPoP %q: want the bearer and no proof", auth, proof)
	}
}

// WithOrigin applies to the load too: htu names the public origin.
func TestFireAs_WithOriginSignsForThePublicOrigin(t *testing.T) {
	const public = "https://api.example.test"
	g := newLoadGateway(t)
	var err error
	nonces, _ := dpop.NewHMACNonces([]byte("0123456789abcdef0123456789abcdef"))
	if g.v, err = dpop.NewVerifier(dpop.Config{Origin: public, Nonces: nonces}); err != nil {
		t.Fatal(err)
	}
	lc := stress.NewLoadCaller(g.URL, 1, 5*time.Second).WithSigner(dpope2e.New(dpope2e.WithOrigin(public)))
	ep := runner.Endpoint{Ref: "auth.SignIn", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/signin", Mint: true}
	code, err := lc.FireAs(nil, ep, map[string]any{"email": "a"}, "", nil)
	if err != nil || code != http.StatusOK || g.refused.Load() != 0 {
		t.Errorf("mint for the public origin: status %d, err %v, refused %d", code, err, g.refused.Load())
	}
}
