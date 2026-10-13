package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

const dpOrigin = "https://api.example.com"

func dpKey(t *testing.T) *dpop.Key {
	t.Helper()
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func dpVerifier(t *testing.T, nonces dpop.NonceSource) *dpop.Verifier {
	t.Helper()
	v, err := dpop.NewVerifier(dpop.Config{Origin: dpOrigin, Nonces: nonces})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// dpReq builds a request carrying `Authorization: DPoP token` (when token is
// set) and a fresh proof signed by key (when key is set).
func dpReq(t *testing.T, key *dpop.Key, method, path, token, nonce string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "DPoP "+token)
	}
	if key != nil {
		p, err := dpop.NewProof(key, method, dpOrigin+path, token, nonce)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("DPoP", p)
	}
	return r
}

// bindings is a token → thumbprint table standing in for the auth backend.
type bindings struct {
	mu  sync.Mutex
	m   map[string]string
	err error
}

func (b *bindings) set(token, thumb string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]string{}
	}
	b.m[token] = thumb
}

func (b *bindings) lookup(_ context.Context, token string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return "", b.err
	}
	return b.m[token], nil
}

func dpGate(t *testing.T, v *dpop.Verifier, b *bindings, opts ...restgw.DPoPOption) *restgw.DPoPGate {
	t.Helper()
	if b != nil {
		opts = append(opts, restgw.WithDPoPBinding(b.lookup))
	}
	g, err := restgw.NewDPoPGate(v, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// serve runs r through the gate's middleware and reports the response and
// what next saw (nil when next did not run).
func serve(g *restgw.DPoPGate, r *http.Request) (*httptest.ResponseRecorder, *restgw.DPoPVerified) {
	var seen *restgw.DPoPVerified
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := restgw.DPoPFromContext(r.Context())
		seen = &v
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, seen
}

func TestNewDPoPGate_NeedsAVerifier(t *testing.T) {
	if _, err := restgw.NewDPoPGate(nil); err == nil {
		t.Fatal("a gate without a verifier was built")
	}
}

// A bound request whose proof verifies and whose key is the token's binding
// reaches next with the verified thumbprint on its context.
func TestDPoPGate_VerifiesABoundRequest(t *testing.T) {
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nil), b)

	rec, seen := serve(g, dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", ""))
	if rec.Code != http.StatusOK || seen == nil {
		t.Fatalf("status %d, next ran = %v; want 200 and next", rec.Code, seen != nil)
	}
	if seen.Thumbprint != key.Thumbprint() || !seen.Bound() || seen.AccessToken != "tok-1" {
		t.Errorf("verified = %+v, want thumbprint %s bound to tok-1", *seen, key.Thumbprint())
	}
}

// Every refusal is answered by the gate with the RFC 9449 challenge and the
// status dpop.HTTPStatus gives, and next never runs.
func TestDPoPGate_RefusalsCarryTheChallenge(t *testing.T) {
	key, other := dpKey(t), dpKey(t)
	used := dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "")

	cases := []struct {
		name       string
		gate       func(b *bindings) *restgw.DPoPGate
		req        func() *http.Request
		wantStatus int
		wantChall  string // substring of WWW-Authenticate; "" = header absent
	}{
		{
			name:       "token without a proof",
			req:        func() *http.Request { return dpReq(t, nil, http.MethodGet, "/v1/notes", "tok-1", "") },
			wantStatus: http.StatusUnauthorized, wantChall: `DPoP algs=`,
		},
		{
			name:       "proof by another key than the binding",
			req:        func() *http.Request { return dpReq(t, other, http.MethodGet, "/v1/notes", "tok-1", "") },
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_token"`,
		},
		{
			name:       "token not DPoP-bound",
			req:        func() *http.Request { return dpReq(t, key, http.MethodGet, "/v1/notes", "tok-unbound", "") },
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_token"`,
		},
		{
			name:       "no binding lookup configured",
			gate:       func(*bindings) *restgw.DPoPGate { return dpGate(t, dpVerifier(t, nil), nil) },
			req:        func() *http.Request { return dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "") },
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_token"`,
		},
		{
			name: "binding lookup refuses the token",
			gate: func(b *bindings) *restgw.DPoPGate {
				b.err = status.Error(codes.Unauthenticated, "revoked")
				return dpGate(t, dpVerifier(t, nil), b)
			},
			req:        func() *http.Request { return dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "") },
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_token"`,
		},
		{
			name: "binding lookup cannot answer",
			gate: func(b *bindings) *restgw.DPoPGate {
				b.err = status.Error(codes.Unavailable, "auth down")
				return dpGate(t, dpVerifier(t, nil), b)
			},
			req:        func() *http.Request { return dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "") },
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "proof next to a bearer token",
			req: func() *http.Request {
				r := dpReq(t, key, http.MethodGet, "/v1/notes", "", "")
				r.Header.Set("Authorization", "Bearer tok-1")
				return r
			},
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_dpop_proof"`,
		},
		{
			name: "two Authorization headers",
			req: func() *http.Request {
				r := dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "")
				r.Header.Add("Authorization", "Bearer x")
				return r
			},
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_dpop_proof"`,
		},
		{
			name: "proof for another path",
			req: func() *http.Request {
				r := dpReq(t, key, http.MethodGet, "/v1/other", "tok-1", "")
				r.URL.Path = "/v1/notes"
				return r
			},
			wantStatus: http.StatusUnauthorized, wantChall: `error="invalid_dpop_proof"`,
		},
		{
			name: "required gate, no DPoP material",
			gate: func(b *bindings) *restgw.DPoPGate {
				return dpGate(t, dpVerifier(t, nil), b, restgw.WithDPoPRequired())
			},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
				r.Header.Set("Authorization", "Bearer tok-1")
				return r
			},
			wantStatus: http.StatusUnauthorized, wantChall: `DPoP algs=`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bindings{}
			b.set("tok-1", key.Thumbprint())
			g := dpGate(t, dpVerifier(t, nil), b)
			if tc.gate != nil {
				g = tc.gate(b)
			}
			rec, seen := serve(g, tc.req())
			if seen != nil {
				t.Fatal("next ran for a refused request")
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			got := rec.Header().Get("WWW-Authenticate")
			switch {
			case tc.wantChall == "" && got != "":
				t.Errorf("WWW-Authenticate %q on a %d", got, rec.Code)
			case tc.wantChall != "" && !strings.Contains(got, tc.wantChall):
				t.Errorf("WWW-Authenticate %q, want it to contain %q", got, tc.wantChall)
			}
			if tc.wantStatus == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if !strings.Contains(rec.Body.String(), `"code"`) {
				t.Errorf("refusal body is not the error envelope: %s", rec.Body)
			}
		})
	}

	// A proof presented again after its reuse window is a replay. The same
	// gate, so the jti store remembers it.
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	now := time.Now()
	clock := func() time.Time { return now }
	v, err := dpop.NewVerifier(dpop.Config{Origin: dpOrigin, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	g := dpGate(t, v, b)
	if rec, _ := serve(g, used.Clone(context.Background())); rec.Code != http.StatusOK {
		t.Fatalf("first presentation: %d", rec.Code)
	}
	now = now.Add(dpop.DefaultReuseWindow + time.Second)
	rec, seen := serve(g, used.Clone(context.Background()))
	if seen != nil || rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_dpop_proof") {
		t.Errorf("replay: status %d, challenge %q, next ran %v", rec.Code, rec.Header().Get("WWW-Authenticate"), seen != nil)
	}
}

// With nonces on, the gate hands out the nonce in the challenge, accepts the
// retry, and rotates the nonce on the success.
func TestDPoPGate_NonceChallengeAndRotation(t *testing.T) {
	nonces, err := dpop.NewHMACNonces([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nonces), b)

	rec, seen := serve(g, dpReq(t, key, http.MethodPost, "/v1/notes", "tok-1", ""))
	if seen != nil || rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "use_dpop_nonce") {
		t.Fatalf("no nonce: status %d, challenge %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	nonce := rec.Header().Get("DPoP-Nonce")
	if nonce == "" {
		t.Fatal("use_dpop_nonce challenge without a DPoP-Nonce")
	}
	rec, seen = serve(g, dpReq(t, key, http.MethodPost, "/v1/notes", "tok-1", nonce))
	if seen == nil || rec.Code != http.StatusOK {
		t.Fatalf("retry with the nonce: status %d", rec.Code)
	}
	if rec.Header().Get("DPoP-Nonce") == "" {
		t.Error("a verified response carries no DPoP-Nonce for the next proof")
	}
}

// A gate that is not required lets a request without DPoP material through
// unchanged, with nothing verified on its context; a proof without a token
// is an unbound (mint) proof.
func TestDPoPGate_OptionalAndMint(t *testing.T) {
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	plain := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
	plain.Header.Set("Authorization", "Bearer tok-1")
	rec, seen := serve(g, plain)
	if rec.Code != http.StatusOK || seen == nil || seen.Thumbprint != "" {
		t.Fatalf("bearer request: status %d, verified %+v", rec.Code, seen)
	}
	if rec.Header().Get("DPoP-Nonce") != "" {
		t.Error("an unverified request got a nonce")
	}

	key := dpKey(t)
	rec, seen = serve(g, dpReq(t, key, http.MethodPost, "/v1/devices", "", ""))
	if rec.Code != http.StatusOK || seen == nil || seen.Thumbprint != key.Thumbprint() || seen.Bound() {
		t.Fatalf("mint request: status %d, verified %+v", rec.Code, seen)
	}
}

// SetDeviceMetadata stamps x-w17-device on the outgoing side and removes it
// for an empty thumbprint, without touching the caller's metadata.
func TestSetDeviceMetadata(t *testing.T) {
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-w17-user", "env", principal.DeviceMetadataKey, "stale"))
	ctx := restgw.SetDeviceMetadata(base, restgw.DPoPVerified{Thumbprint: "jkt-1", AccessToken: "tok-1"})
	md, _ := metadata.FromOutgoingContext(ctx)
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 1 || got[0] != "jkt-1" {
		t.Errorf("device = %v, want [jkt-1]", got)
	}
	if got := md.Get("x-w17-user"); len(got) != 1 {
		t.Errorf("envelope lost: %v", got)
	}
	orig, _ := metadata.FromOutgoingContext(base)
	if got := orig.Get(principal.DeviceMetadataKey); got[0] != "stale" {
		t.Errorf("caller's metadata modified: %v", got)
	}
	md, _ = metadata.FromOutgoingContext(restgw.SetDeviceMetadata(base, restgw.DPoPVerified{}))
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 0 {
		t.Errorf("empty thumbprint left %v", got)
	}
	md, _ = metadata.FromOutgoingContext(restgw.SetDeviceMetadata(context.Background(), restgw.DPoPVerified{Thumbprint: "jkt-2", AccessToken: "tok-2"}))
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 1 || got[0] != "jkt-2" {
		t.Errorf("no prior metadata: device = %v", got)
	}
}

// A DPoPError keeps its cause for errors.Is and maps to the status the gate
// would answer with, through WriteAuthError too.
func TestDPoPError_Status(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
		http int
	}{
		{dpop.ErrBindingMismatch, codes.Unauthenticated, http.StatusUnauthorized},
		{dpop.ErrUnavailable, codes.Unavailable, http.StatusServiceUnavailable},
		{errors.New("boom"), codes.Internal, http.StatusInternalServerError},
	} {
		e := &restgw.DPoPError{Err: tc.err}
		if !errors.Is(e, tc.err) {
			t.Errorf("errors.Is lost the cause %v", tc.err)
		}
		if got := status.Code(e); got != tc.code {
			t.Errorf("%v: code %v, want %v", tc.err, got, tc.code)
		}
		rec := httptest.NewRecorder()
		restgw.WriteAuthError(rec, e)
		if rec.Code != tc.http {
			t.Errorf("%v: WriteAuthError %d, want %d", tc.err, rec.Code, tc.http)
		}
	}
}

// --- auth cache ---

// dpCountingAuth answers with a principal naming the request's verified
// thumbprint (or "none") and counts its calls.
type dpCountingAuth struct{ calls atomic.Int32 }

func (c *dpCountingAuth) fn(_ context.Context, r *http.Request) ([]byte, error) {
	c.calls.Add(1)
	v, _ := restgw.DPoPFromContext(r.Context())
	return []byte("principal-for:" + v.Thumbprint), nil
}

// authThrough runs the cached AuthFunc behind the gate, the way a generated
// route mounts them, and returns what it answered.
func authThrough(t *testing.T, g *restgw.DPoPGate, authFn restgw.AuthFunc, r *http.Request) (int, string) {
	t.Helper()
	var got string
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := authFn(r.Context(), r)
		if err != nil {
			t.Fatalf("auth: %v", err)
		}
		got = string(data)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, got
}

// A verified request is keyed on the verified thumbprint: one key's fresh
// proofs hit its entry, another key presenting the same credential headers
// never reads it.
func TestCachedDPoPAuthFunc_KeysOnTheVerifiedThumbprint(t *testing.T) {
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	inner := &dpCountingAuth{}
	cached := restgw.CachedDPoPAuthFunc(inner.fn, time.Minute, nil)

	a, b := dpKey(t), dpKey(t)
	mint := func(k *dpop.Key) *http.Request {
		r := dpReq(t, k, http.MethodPost, "/v1/devices", "", "")
		r.Header.Set("Cookie", "session=shared")
		return r
	}
	for i := range 3 {
		code, got := authThrough(t, g, cached, mint(a))
		if code != http.StatusOK || got != "principal-for:"+a.Thumbprint() {
			t.Fatalf("key A, call %d: %d %q", i, code, got)
		}
	}
	if n := inner.calls.Load(); n != 1 {
		t.Errorf("inner called %d times for one key's fresh proofs, want 1 (the proof header must not defeat the cache)", n)
	}
	_, got := authThrough(t, g, cached, mint(b))
	if got != "principal-for:"+b.Thumbprint() {
		t.Fatalf("key B with the same cookie was answered %q: another device's cached principal", got)
	}
}

// DPoP material that was not verified on this request — the gate not
// mounted, or the cached AuthFunc called directly — is never answered from
// the cache and never fills it.
func TestCachedDPoPAuthFunc_NeverAnswersAnUnverifiedRequest(t *testing.T) {
	key := dpKey(t)
	b := &bindings{}
	b.set("tok-1", key.Thumbprint())
	g := dpGate(t, dpVerifier(t, nil), b)
	inner := &dpCountingAuth{}
	cached := restgw.CachedDPoPAuthFunc(inner.fn, time.Minute, nil)

	if code, _ := authThrough(t, g, cached, dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "")); code != http.StatusOK {
		t.Fatalf("priming request: %d", code)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("inner calls %d, want 1", inner.calls.Load())
	}

	thief := dpKey(t)
	for name, r := range map[string]*http.Request{
		"stolen token, own proof, no gate": dpReq(t, thief, http.MethodGet, "/v1/notes", "tok-1", ""),
		"stolen token, no proof":           dpReq(t, nil, http.MethodGet, "/v1/notes", "tok-1", ""),
		"empty DPoP scheme": func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
			r.Header.Set("Authorization", "DPoP")
			return r
		}(),
	} {
		// Twice: the second presentation must not be served from an entry
		// the first one filled.
		for range 2 {
			before := inner.calls.Load()
			data, err := cached(r.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) == "principal-for:"+key.Thumbprint() {
				t.Errorf("%s: answered with the cached principal of the real device", name)
			}
			if inner.calls.Load() != before+1 {
				t.Errorf("%s: inner not called; an unverified request was served from the cache", name)
			}
		}
	}
	// And they did not fill the cache: the verified device still hits.
	before := inner.calls.Load()
	if _, got := authThrough(t, g, cached, dpReq(t, key, http.MethodGet, "/v1/notes", "tok-1", "")); got != "principal-for:"+key.Thumbprint() {
		t.Errorf("verified device answered %q", got)
	}
	if inner.calls.Load() != before {
		t.Error("the verified device's entry was evicted or overwritten by unverified requests")
	}
}

// A request with no DPoP material is cached as CachedAuthFuncWithKeyHeaders
// caches it, and ttl <= 0 is no cache at all.
func TestCachedDPoPAuthFunc_BearerAndOff(t *testing.T) {
	inner := &dpCountingAuth{}
	cached := restgw.CachedDPoPAuthFunc(inner.fn, time.Minute, nil)
	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
		r.Header.Set("Authorization", "Bearer tok-1")
		if _, err := cached(r.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if n := inner.calls.Load(); n != 1 {
		t.Errorf("bearer requests: inner called %d times, want 1", n)
	}
	off := restgw.CachedDPoPAuthFunc(inner.fn, 0, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
	r.Header.Set("Authorization", "Bearer tok-1")
	before := inner.calls.Load()
	_, _ = off(r.Context(), r)
	_, _ = off(r.Context(), r)
	if inner.calls.Load() != before+2 {
		t.Error("ttl 0 still cached")
	}
}

// A proof with no token — a cookie request, or no credential at all — proves
// only that the client holds a key it may have generated a moment ago. It
// must never become a device id: DeviceID is empty, SetDeviceMetadata stamps
// nothing (and removes a stale copy), and principal.DeviceID reads false.
func TestDPoPGate_UnboundProofIsNoDevice(t *testing.T) {
	g := dpGate(t, dpVerifier(t, nil), &bindings{})
	fresh := dpKey(t)
	r := dpReq(t, fresh, http.MethodGet, "/v1/notes", "", "")
	r.Header.Set("Cookie", "session=s1")
	rec, seen := serve(g, r)
	if rec.Code != http.StatusOK || seen == nil {
		t.Fatalf("cookie + unbound proof: status %d", rec.Code)
	}
	if seen.DeviceID() != "" {
		t.Errorf("an unbound proof yields device id %q", seen.DeviceID())
	}
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(principal.DeviceMetadataKey, "stale"))
	md, _ := metadata.FromOutgoingContext(restgw.SetDeviceMetadata(base, *seen))
	if got := md.Get(principal.DeviceMetadataKey); len(got) != 0 {
		t.Fatalf("SetDeviceMetadata stamped %v for a key the client just generated", got)
	}
	if dev, ok := principal.DeviceID(metadata.NewIncomingContext(context.Background(), md)); ok {
		t.Errorf("principal.DeviceID = %q for an unbound proof", dev)
	}
	if (restgw.DPoPVerified{Thumbprint: "jkt", AccessToken: "tok"}).DeviceID() != "jkt" {
		t.Error("a bound verification lost its device id")
	}
}
