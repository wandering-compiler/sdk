package dpope2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-jose/go-jose/v4"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/dpope2e"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// dpopGateway fakes a gateway whose auth method binds devices. It verifies
// every proof with lib/dpop's verifier, with nonces on (so the first proof
// of each key meets use_dpop_nonce):
//
//	POST /signin  mint: an unbound proof and no token; answers a token bound
//	              to the proof's key, and that key's thumbprint as jkt — or,
//	              for an email starting "mfa", an MFA challenge bound to it
//	POST /verify  mint: completes a challenge; a proof by another key than
//	              the challenge's is refused (OTHER_DEVICE)
//	POST /tokens  issues a token bound to the request's public_key JWK
//	GET  /go      redirects to redirectTo, whatever the request carries
//	GET  /me      a bound call: answers the thumbprint the proof was signed by
//	POST /revoke  revokes the caller's device
//	POST /ping    fans an event out to every /w17-events subscriber
//	GET  /stream  an SSE stream of one frame carrying the caller's jkt
//	GET  /w17-events  SSE; each ping becomes a frame carrying the
//	              subscriber's own jkt
//
// Any request presenting `Bearer`, or a DPoP token without a proof by its
// key, is refused.
type dpopGateway struct {
	*httptest.Server
	t *testing.T
	v *dpop.Verifier

	mu       sync.Mutex
	tokens   map[string]string // token → jkt
	revoked  map[string]bool   // jkt
	seq      int
	requests []seenRequest
	subs     map[chan string]string // subscriber → its jkt
	mfa      map[string]string      // challenge → jkt

	redirectTo string
}

type seenRequest struct {
	path, authorization string
	proof               bool
	status              int
}

func newDPoPGateway(t *testing.T) *dpopGateway { return newDPoPGatewayAt(t, "") }

// newDPoPGatewayAt is a gateway whose verifier expects proofs for the public
// origin, not for the address it listens on ("" = that address).
func newDPoPGatewayAt(t *testing.T, origin string) *dpopGateway {
	t.Helper()
	g := &dpopGateway{t: t, tokens: map[string]string{}, revoked: map[string]bool{}, subs: map[chan string]string{}, mfa: map[string]string{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	nonces, err := dpop.NewHMACNonces([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if origin == "" {
		origin = g.URL
	}
	if g.v, err = dpop.NewVerifier(dpop.Config{Origin: origin, Nonces: nonces}); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *dpopGateway) record(r *http.Request, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, seenRequest{
		path: r.URL.Path, authorization: r.Header.Get("Authorization"),
		proof: r.Header.Get(dpop.HeaderProof) != "", status: status,
	})
}

func (g *dpopGateway) seen(path string) []seenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []seenRequest
	for _, s := range g.requests {
		if s.path == path {
			out = append(out, s)
		}
	}
	return out
}

func refuseDPoP(w http.ResponseWriter, code, detail, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	d := ""
	if detail != "" {
		d = `,"details":[{"code":"` + detail + `"}]`
	}
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q%s}}`, code, msg, d)
}

func (g *dpopGateway) serve(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	defer func() { g.record(r, status) }()

	if r.URL.Path == "/go" {
		status = http.StatusFound
		http.Redirect(w, r, g.redirectTo, http.StatusFound)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	mint := r.URL.Path == "/signin" || r.URL.Path == "/verify"
	tok, isDPoP := dpop.AccessToken(r.Header.Get("Authorization"))
	if !mint && !isDPoP {
		status = http.StatusUnauthorized
		refuseDPoP(w, "UNAUTHENTICATED", "", "want Authorization: DPoP, got "+r.Header.Get("Authorization"))
		return
	}
	if mint && r.Header.Get("Authorization") != "" {
		status = http.StatusUnauthorized
		refuseDPoP(w, "UNAUTHENTICATED", "", "a mint carries no token")
		return
	}
	g.mu.Lock()
	binding, known := g.tokens[tok]
	g.mu.Unlock()
	if !mint && !known {
		status = http.StatusUnauthorized
		refuseDPoP(w, "UNAUTHENTICATED", "", "unknown token")
		return
	}
	jkt, err := g.v.Verify(r.Context(), dpop.Request{
		Method: r.Method, Path: r.URL.EscapedPath(), Proofs: r.Header.Values(dpop.HeaderProof),
		AccessToken: tok, Binding: binding,
	})
	if err != nil {
		status = http.StatusUnauthorized
		dpop.SetChallenge(w.Header(), err)
		refuseDPoP(w, "UNAUTHENTICATED", "", err.Error())
		return
	}
	if n, err := g.v.Nonce(jkt); err == nil {
		w.Header().Set(dpop.HeaderNonce, n)
	}
	g.mu.Lock()
	revoked := g.revoked[jkt]
	g.mu.Unlock()
	if revoked {
		status = http.StatusUnauthorized
		refuseDPoP(w, "UNAUTHENTICATED", "DEVICE_REVOKED", "device revoked")
		return
	}

	switch r.URL.Path {
	case "/signin":
		if email, _ := body["email"].(string); strings.HasPrefix(email, "mfa") {
			g.mu.Lock()
			g.seq++
			ch := fmt.Sprintf("ch-%d", g.seq)
			g.mfa[ch] = jkt
			g.mu.Unlock()
			writeJSON(w, map[string]any{"mfa": map[string]any{"challenge_id": ch}})
			return
		}
		writeJSON(w, map[string]any{"token": g.issue(jkt), "jkt": jkt})
	case "/verify":
		ch, _ := body["challenge_id"].(string)
		g.mu.Lock()
		want, ok := g.mfa[ch]
		g.mu.Unlock()
		if !ok || want != jkt {
			status = http.StatusUnauthorized
			refuseDPoP(w, "UNAUTHENTICATED", "OTHER_DEVICE", "the challenge belongs to another device")
			return
		}
		writeJSON(w, map[string]any{"token": g.issue(jkt), "jkt": jkt})
	case "/tokens":
		pk, _ := body["public_key"].(string)
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON([]byte(pk)); err != nil || !jwk.IsPublic() || strings.Contains(pk, `"d"`) {
			status = http.StatusBadRequest
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"INVALID_ARGUMENT","message":"public_key"}}`)
			return
		}
		tp, err := dpop.Thumbprint(jwk.Key)
		if err != nil {
			status = http.StatusBadRequest
			w.WriteHeader(status)
			return
		}
		writeJSON(w, map[string]any{"token": g.issue(tp), "device_id": tp})
	case "/me":
		writeJSON(w, map[string]any{"jkt": jkt})
	case "/revoke":
		g.mu.Lock()
		g.revoked[jkt] = true
		g.mu.Unlock()
		writeJSON(w, map[string]any{})
	case "/ping":
		g.mu.Lock()
		for ch, sub := range g.subs {
			ch <- sub
		}
		g.mu.Unlock()
		writeJSON(w, map[string]any{})
	case "/stream":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: frame\ndata: {\"jkt\":%q}\n\n", jkt)
	case "/w17-events":
		ch := make(chan string, 4)
		g.mu.Lock()
		g.subs[ch] = jkt
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			delete(g.subs, ch)
			g.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case sub := <-ch:
			_, _ = fmt.Fprintf(w, "event: pinged\ndata: {\"sub_jkt\":%q}\n\n", sub)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
	default:
		status = http.StatusNotFound
		http.NotFound(w, r)
	}
}

func (g *dpopGateway) issue(jkt string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	tok := fmt.Sprintf("tok-%d", g.seq)
	g.tokens[tok] = jkt
	return tok
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func dpopSignIn(email string, expect map[string]any) runner.Step {
	return runner.Step{
		Label:    "signin " + email,
		Endpoint: runner.Endpoint{Ref: "auth.SignIn", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/signin", Mint: true},
		Input:    map[string]any{"email": email},
		Expect:   expect,
	}
}

func dpopMe(tokenFrom, captureAs string) runner.Step {
	return runner.Step{
		Label:     "me",
		Endpoint:  runner.Endpoint{Ref: "me.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/me", AuthRequired: true},
		TokenFrom: tokenFrom,
		Expect:    map[string]any{"jkt": map[string]any{"capture": captureAs}},
	}
}

func dpopCallers(g *dpopGateway) map[string]runner.Caller {
	return map[string]runner.Caller{"rest": runner.NewRESTCaller(g.URL, nil).WithSigner(dpope2e.New())}
}

func captured(t *testing.T, scope *runtime.Scope, name string) string {
	t.Helper()
	v, ok := scope.Get(name)
	if !ok {
		t.Fatalf("%s not captured", name)
	}
	return runtime.Text(v)
}

// A sign-in signs an unbound proof and sends no token; the token it mints is
// then presented as `DPoP` with proofs by the same key, through the
// gateway's nonce challenge.
func TestDPoP_SignInThenCallSignsWithTheMintingKey(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "mint_jkt"}}),
		dpopMe("", "me_jkt"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	if mintJKT, meJKT := captured(t, scope, "mint_jkt"), captured(t, scope, "me_jkt"); mintJKT != meJKT {
		t.Errorf("the call was signed by %s, the token was minted for %s", meJKT, mintJKT)
	}
	me := g.seen("/me")
	if last := me[len(me)-1]; last.authorization != "DPoP "+captured(t, scope, "auth.token") || !last.proof {
		t.Errorf("/me carried Authorization %q (proof %v), want the DPoP token with a proof", last.authorization, last.proof)
	}
	// The first proof of the key met use_dpop_nonce and was retried.
	if signin := g.seen("/signin"); len(signin) != 2 || signin[0].status != http.StatusUnauthorized || signin[1].status != http.StatusOK {
		t.Errorf("sign-in requests %+v, want a nonce challenge then success", signin)
	}
}

// A mint step never presents the stored token, even when the endpoint is
// auth-required and a session is already bound: the edge would check the
// stale token before the sign-in runs.
func TestDPoP_MintSendsNoStoredToken(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	again := dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}})
	again.Endpoint.AuthRequired = true
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}}),
		again,
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	for _, s := range g.seen("/signin") {
		if s.authorization != "" || !s.proof {
			t.Errorf("a sign-in carried Authorization %q (proof %v), want no token and a proof", s.authorization, s.proof)
		}
	}
}

// Each principal's token is signed with the key it was minted for; signing
// in again into the same slot reuses the device, and Step.Device makes a
// second device of the same principal.
func TestDPoP_KeyPerPrincipal(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	phone := dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "phone.token"}, "jkt": map[string]any{"capture": "phone_mint"}})
	phone.Device = "phone"
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "alice_mint"}}),
		dpopSignIn("bob", map[string]any{"token": map[string]any{"capture": "bob.token"}, "jkt": map[string]any{"capture": "bob_mint"}}),
		dpopMe("bob.token", "bob_me"),
		dpopMe("", "alice_me"),
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "alice_again"}}),
		dpopMe("", "alice_me2"),
		phone,
		dpopMe("phone.token", "phone_me"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	alice, bob, phoneKey := captured(t, scope, "alice_mint"), captured(t, scope, "bob_mint"), captured(t, scope, "phone_mint")
	if alice == bob || alice == phoneKey || bob == phoneKey {
		t.Errorf("devices alice %s, bob %s, phone %s: want three keys", alice, bob, phoneKey)
	}
	for name, want := range map[string]string{"bob_me": bob, "alice_me": alice, "alice_again": alice, "alice_me2": alice, "phone_me": phoneKey} {
		if got := captured(t, scope, name); got != want {
			t.Errorf("%s signed by %s, want %s", name, got, want)
		}
	}
}

// Two runs are two sets of devices: a scenario never signs with another
// scenario's key.
func TestDPoP_RunsDoNotShareKeys(t *testing.T) {
	g := newDPoPGateway(t)
	callers := dpopCallers(g)
	var jkts []string
	for i := 0; i < 2; i++ {
		scope := runtime.NewRun().NewScope()
		if err := runner.RunSteps(context.Background(), scope, []runner.Step{
			dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "jkt"}}),
		}, callers); err != nil {
			t.Fatal(err)
		}
		jkts = append(jkts, captured(t, scope, "jkt"))
	}
	if jkts[0] == jkts[1] {
		t.Errorf("two runs signed with one key %s", jkts[0])
	}
}

// A revoked device's refusal reaches the scenario with its detail code — on
// every call presenting its token, not only the first — and the device's
// next sign-in signs with a new key, so it works.
func TestDPoP_DeviceRevoked(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	revoke := runner.Step{Label: "revoke", Endpoint: runner.Endpoint{Ref: "devices.Revoke", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/revoke", AuthRequired: true}}
	refused := dpopMe("auth.token", "")
	refused.Expect = nil
	refused.ExpectError = &runner.ExpectError{Code: "UNAUTHENTICATED", Details: []runner.ExpectErrorDetail{{Code: "DEVICE_REVOKED"}}}
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "first"}}),
		revoke,
		refused,
		refused,
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "second"}}),
		dpopMe("", "me"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	if first, second := captured(t, scope, "first"), captured(t, scope, "second"); first == second {
		t.Errorf("signed in again with the revoked key %s", first)
	}
	if me, second := captured(t, scope, "me"), captured(t, scope, "second"); me != second {
		t.Errorf("after the new sign-in /me was signed by %s, want %s", me, second)
	}
}

// A step credential of the DPoP scheme is signed for with the key that
// minted its token.
func TestDPoP_DPoPCredentialIsSigned(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	cred := dpopMe("", "cred_me")
	cred.Credential = "DPoP ${bob.token}"
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}}),
		dpopSignIn("bob", map[string]any{"token": map[string]any{"capture": "bob.token"}, "jkt": map[string]any{"capture": "bob_mint"}}),
		cred,
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	if got, want := captured(t, scope, "cred_me"), captured(t, scope, "bob_mint"); got != want {
		t.Errorf("credential call signed by %s, want bob's %s", got, want)
	}
}

// A server stream is opened with the session token and a proof by its key.
func TestDPoP_StreamIsSigned(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "mint"}}),
		{
			Label:        "watch",
			Endpoint:     runner.Endpoint{Ref: "feed.Watch", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/stream", AuthRequired: true, Stream: true},
			ExpectStream: &runner.ExpectStream{Frames: []map[string]any{{"jkt": map[string]any{"capture": "frame_jkt"}}}},
		},
	}
	err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g),
		runner.WithStreamCaller(runner.NewSSEStreamCaller(g.URL, nil).WithSigner(dpope2e.New())))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := captured(t, scope, "frame_jkt"), captured(t, scope, "mint"); got != want {
		t.Errorf("stream opened by %s, want %s", got, want)
	}
}

// Event subscriptions sign as the principal they observe: the step's own
// session, or the await's token_from.
func TestDPoP_EventSubscriptionsAreSigned(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	ping := runner.Step{
		Label:    "ping",
		Endpoint: runner.Endpoint{Ref: "ping.Ping", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/ping", AuthRequired: true},
		AwaitEvents: []runner.AwaitEvent{
			{Topic: "pinged", Path: "/w17-events", Match: map[string]any{"sub_jkt": map[string]any{"capture": "own_sub"}}},
			{Topic: "pinged", Path: "/w17-events", TokenFrom: "bob.token", Match: map[string]any{"sub_jkt": map[string]any{"capture": "bob_sub"}}},
		},
	}
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "alice"}}),
		dpopSignIn("bob", map[string]any{"token": map[string]any{"capture": "bob.token"}, "jkt": map[string]any{"capture": "bob"}}),
		ping,
	}
	err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g),
		runner.WithEventSubscriber(runner.NewSSESubscriber(g.URL, nil).WithSigner(dpope2e.New())))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := captured(t, scope, "own_sub"), captured(t, scope, "alice"); got != want {
		t.Errorf("own subscription signed by %s, want %s", got, want)
	}
	if got, want := captured(t, scope, "bob_sub"), captured(t, scope, "bob"); got != want {
		t.Errorf("token_from subscription signed by %s, want %s", got, want)
	}
}

// headerRecorder answers every request with a token and records each
// request's full header set.
func headerRecorder(t *testing.T) (*httptest.Server, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: frame\ndata: {\"ok\":true}\n\n")
			return
		}
		writeJSON(w, map[string]any{"token": "tok-1"})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), seen...)
	}
}

// Without WithSigner the wire is what it was: Bearer, no DPoP header, a mint
// endpoint sent like any other public call — every header of every request
// pinned.
func TestDPoP_OffModeWireIsUnchanged(t *testing.T) {
	srv, seen := headerRecorder(t)
	scope := runtime.NewRun().NewScope()
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}}),
		{Label: "me", Endpoint: runner.Endpoint{Ref: "me.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/me", AuthRequired: true}},
		{Label: "cred", Endpoint: runner.Endpoint{Ref: "me.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/me", AuthRequired: true}, Credential: "DPoP raw"},
		{
			Label:        "watch",
			Endpoint:     runner.Endpoint{Ref: "feed.Watch", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/stream", AuthRequired: true, Stream: true},
			ExpectStream: &runner.ExpectStream{Frames: []map[string]any{{"ok": true}}},
		},
	}
	err := runner.RunSteps(context.Background(), scope, steps,
		map[string]runner.Caller{"rest": runner.NewRESTCaller(srv.URL, nil)},
		runner.WithStreamCaller(runner.NewSSEStreamCaller(srv.URL, nil)))
	if err != nil {
		t.Fatal(err)
	}
	want := []http.Header{
		{"Accept-Encoding": {"gzip"}, "Content-Length": {"17"}, "Content-Type": {"application/json"}, "User-Agent": {"Go-http-client/1.1"}},
		{"Accept-Encoding": {"gzip"}, "Authorization": {"Bearer tok-1"}, "User-Agent": {"Go-http-client/1.1"}},
		{"Accept-Encoding": {"gzip"}, "Authorization": {"DPoP raw"}, "User-Agent": {"Go-http-client/1.1"}},
		{"Accept": {"text/event-stream"}, "Accept-Encoding": {"gzip"}, "Authorization": {"Bearer tok-1"}, "User-Agent": {"Go-http-client/1.1"}},
	}
	got := seen()
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Errorf("request %d headers\n got %v\nwant %v", i, got[i], want[i])
		}
	}

	// The event subscriber too.
	sub, err := runner.NewSSESubscriber(srv.URL, nil).Subscribe(context.Background(), "/w17-events", []string{"x"}, "tok-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = sub.Close()
	got = seen()
	last := got[len(got)-1]
	if last.Get("Authorization") != "Bearer tok-1" || last.Get(dpop.HeaderProof) != "" {
		t.Errorf("subscriber headers %v, want the bearer and no proof", last)
	}
}

// A signing caller sends nothing DPoP on a call that has neither a token
// nor a mint: anonymous browsing costs no proof.
func TestDPoP_AnonymousCallCarriesNoProof(t *testing.T) {
	srv, seen := headerRecorder(t)
	c := runner.NewRESTCaller(srv.URL, nil).WithSigner(dpope2e.New())
	ep := runner.Endpoint{Ref: "pub.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/pub"}
	if _, err := c.Call(context.Background(), ep, nil, "", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	h := seen()[0]
	if h.Get("Authorization") != "" || h.Get(dpop.HeaderProof) != "" {
		t.Errorf("anonymous call carried %v", h)
	}

	// Nothing about signing is consulted for it: not even a signer whose
	// configuration fails every signed call.
	bad := runner.NewRESTCaller(srv.URL, nil).WithSigner(dpope2e.New(dpope2e.WithOrigin("not an origin")))
	if _, err := bad.Call(context.Background(), ep, nil, "", nil, "", nil); err != nil {
		t.Errorf("an anonymous call failed on the signer's configuration: %v", err)
	}
}

// An MFA sign-in is two mints by one device: VerifyMfa sends the
// challenge SignIn answered, captures no token itself on SignIn, and must
// be signed by SignIn's key or the gateway refuses it as another device.
func TestDPoP_MFASignInSignsBothMintsWithOneKey(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	verify := runner.Step{
		Label:    "verify",
		Endpoint: runner.Endpoint{Ref: "auth.VerifyMfa", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/verify", Mint: true},
		Input:    map[string]any{"challenge_id": "${mfa.challenge_id}", "code": "123456"},
		Expect:   map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "verify_jkt"}},
	}
	steps := []runner.Step{
		dpopSignIn("mfa-alice", map[string]any{"mfa": map[string]any{"challenge_id": map[string]any{"capture": "mfa.challenge_id"}}}),
		verify,
		dpopMe("", "me_jkt"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	if got, want := captured(t, scope, "me_jkt"), captured(t, scope, "verify_jkt"); got != want {
		t.Errorf("the session was signed by %s, minted for %s", got, want)
	}
}

// `${device_jwk:<name>}` sends a device's public key; the token the step
// captures is bound to that key, and token_from it signs with it.
func TestDPoP_DeviceJWKBindsAnAPIToken(t *testing.T) {
	g := newDPoPGateway(t)
	scope := runtime.NewRun().NewScope()
	create := runner.Step{
		Label:    "create token",
		Endpoint: runner.Endpoint{Ref: "auth.CreateApiToken", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/tokens", AuthRequired: true},
		Input:    map[string]any{"public_key": "${device_jwk:ci-bot}"},
		Expect:   map[string]any{"token": map[string]any{"capture": "api.token"}, "device_id": map[string]any{"capture": "api_device"}},
	}
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "session"}}),
		create,
		dpopMe("api.token", "api_me"),
		dpopMe("", "session_me"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	api, session := captured(t, scope, "api_device"), captured(t, scope, "session")
	if api == session {
		t.Fatal("the API token was bound to the session's key")
	}
	if got := captured(t, scope, "api_me"); got != api {
		t.Errorf("the API token was presented with a proof by %s, want %s", got, api)
	}
	if got := captured(t, scope, "session_me"); got != session {
		t.Errorf("the session was presented with a proof by %s, want %s", got, session)
	}
}

func TestPublicJWK_HasNoPrivateMember(t *testing.T) {
	s := dpope2e.New()
	var dv runner.Device
	j, err := s.PublicJWK(&dv)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatal(err)
	}
	for _, priv := range []string{"d", "p", "q", "dp", "dq", "qi", "k"} {
		if _, ok := m[priv]; ok {
			t.Errorf("JWK carries private member %q: %s", priv, j)
		}
	}
	if m["kty"] != "EC" || m["crv"] != "P-256" {
		t.Errorf("JWK %s, want a P-256 key", j)
	}
	if again, _ := s.PublicJWK(&dv); again != j {
		t.Error("one device answered two keys")
	}
}

// ${device_jwk:…} on a caller that does not sign is an error, not a guess.
func TestDPoP_DeviceJWKNeedsASigningCaller(t *testing.T) {
	srv, _ := headerRecorder(t)
	step := runner.Step{
		Label:    "create token",
		Endpoint: runner.Endpoint{Ref: "auth.CreateApiToken", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/tokens"},
		Input:    map[string]any{"public_key": "${device_jwk:bot}"},
	}
	err := runner.RunSteps(context.Background(), runtime.NewRun().NewScope(), []runner.Step{step},
		map[string]runner.Caller{"rest": runner.NewRESTCaller(srv.URL, nil)})
	if err == nil || !strings.Contains(err.Error(), "does not sign") {
		t.Errorf("err = %v, want the caller named as not signing", err)
	}
}

// A redirect to another origin (a presigned URL) is followed: an anonymous
// call as before, and a signed call without its token or a proof.
func TestDPoP_CrossOriginRedirectGoesUnsigned(t *testing.T) {
	var mu sync.Mutex
	var got []http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		writeJSON(w, map[string]any{"file": "ok"})
	}))
	defer other.Close()
	g := newDPoPGateway(t)
	g.redirectTo = other.URL + "/dl"

	scope := runtime.NewRun().NewScope()
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}}),
		{Label: "anon download", Endpoint: runner.Endpoint{Ref: "files.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/go"}, Expect: map[string]any{"file": "ok"}},
		{Label: "signed download", Endpoint: runner.Endpoint{Ref: "files.Get", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/go", AuthRequired: true}, Expect: map[string]any{"file": "ok"}},
	}
	if err := runner.RunSteps(context.Background(), scope, steps, dpopCallers(g)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the other origin saw %d requests, want 2", len(got))
	}
	for i, h := range got {
		if h.Get("Authorization") != "" || h.Get(dpop.HeaderProof) != "" {
			t.Errorf("redirected request %d carried Authorization %q, DPoP %q", i, h.Get("Authorization"), h.Get(dpop.HeaderProof))
		}
	}
	// The signed call itself was signed at home.
	if gos := g.seen("/go"); len(gos) < 2 || gos[len(gos)-1].authorization == "" || !gos[len(gos)-1].proof {
		t.Errorf("the signed call reached the gateway as %+v", gos)
	}
}

// WithOrigin signs for the gateway's public origin while dialling the
// address the runner reaches it by.
func TestDPoP_WithOriginSignsForThePublicOrigin(t *testing.T) {
	const public = "https://api.example.test"
	g := newDPoPGatewayAt(t, public)
	scope := runtime.NewRun().NewScope()
	s := dpope2e.New(dpope2e.WithOrigin(public))
	callers := map[string]runner.Caller{"rest": runner.NewRESTCaller(g.URL, nil).WithSigner(s)}
	steps := []runner.Step{
		dpopSignIn("alice", map[string]any{"token": map[string]any{"capture": "auth.token"}, "jkt": map[string]any{"capture": "mint"}}),
		dpopMe("", "me"),
	}
	if err := runner.RunSteps(context.Background(), scope, steps, callers); err != nil {
		t.Fatal(err)
	}
	if captured(t, scope, "me") != captured(t, scope, "mint") {
		t.Error("the session was not signed by its key")
	}

	// Without the option the proofs name the dialled address and fail.
	err := runner.RunSteps(context.Background(), runtime.NewRun().NewScope(), steps[:1], dpopCallers(g))
	if err == nil {
		t.Error("proofs for the dialled address passed a gateway that expects its public origin")
	}

	// A bad WithOrigin fails every signed call, even against a gateway that
	// would accept proofs for the dialled address.
	plain := newDPoPGateway(t)
	_, err = runner.NewRESTCaller(plain.URL, nil).WithSigner(dpope2e.New(dpope2e.WithOrigin("not an origin"))).
		Call(context.Background(), steps[0].Endpoint, map[string]any{"email": "a"}, "", nil, "", nil)
	if err == nil || !strings.Contains(err.Error(), "WithOrigin") {
		t.Errorf("a bad WithOrigin: err = %v, want it named", err)
	}
}
