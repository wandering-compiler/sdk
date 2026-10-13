package dpopclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
)

var nonceKey = []byte("0123456789abcdef0123456789abcdef")

func mustKey(t *testing.T) *dpop.Key {
	t.Helper()
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// server is a resource server that verifies with nonces on and binds the
// token "tok" to the key bound.
type server struct {
	*httptest.Server
	v      *dpop.Verifier
	hits   atomic.Int32
	mu     sync.Mutex
	bodies []string
	// alwaysNonce refuses every request with a new nonce.
	alwaysNonce bool
	binding     string
}

func newServer(t *testing.T, binding string) *server {
	t.Helper()
	s := &server{binding: binding}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	nonces, err := dpop.NewHMACNonces(nonceKey)
	if err != nil {
		t.Fatal(err)
	}
	s.v, err = dpop.NewVerifier(dpop.Config{Origin: s.URL, Nonces: nonces})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	s.mu.Unlock()
	tok, _ := dpop.AccessToken(r.Header.Get("Authorization"))
	binding := ""
	if tok != "" {
		binding = s.binding
	}
	tp, err := s.v.Verify(r.Context(), dpop.Request{
		Method: r.Method, Path: r.URL.EscapedPath(), Proofs: r.Header.Values("DPoP"),
		AccessToken: tok, Binding: binding,
	})
	if err == nil && s.alwaysNonce {
		n, _ := s.v.Nonce(tp + string(rune('a'+s.hits.Load())))
		err = &dpop.NonceError{Nonce: n}
	}
	if err != nil {
		dpop.SetChallenge(w.Header(), err)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"UNAUTHENTICATED","message":"`+err.Error()+`"}}`)
		return
	}
	_, _ = io.WriteString(w, "ok:"+tp)
}

func token(tok string) dpopclient.TokenFunc {
	return func(context.Context) (string, error) { return tok, nil }
}

func do(t *testing.T, c *http.Client, method, url, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// The first request has no nonce; the server answers use_dpop_nonce, the
// transport retries once with it, and later requests carry it from the start.
func TestTransport_NonceRetryOnce(t *testing.T) {
	k := mustKey(t)
	s := newServer(t, k.Thumbprint())
	c := &http.Client{Transport: &dpopclient.Transport{Signer: k, Token: token("tok")}}

	resp := do(t, c, "POST", s.URL+"/things?x=1", "payload")
	if got := readAll(t, resp); resp.StatusCode != 200 || got != "ok:"+k.Thumbprint() {
		t.Fatalf("first request = %d %q", resp.StatusCode, got)
	}
	if s.hits.Load() != 2 {
		t.Fatalf("server saw %d attempts, want 2 (refusal + one retry)", s.hits.Load())
	}
	s.mu.Lock()
	bodies := append([]string(nil), s.bodies...)
	s.mu.Unlock()
	if bodies[0] != "payload" || bodies[1] != "payload" {
		t.Fatalf("bodies = %q, want the payload on both attempts", bodies)
	}
	resp = do(t, c, "GET", s.URL+"/things", "")
	if resp.StatusCode != 200 {
		t.Fatalf("second request = %d", resp.StatusCode)
	}
	readAll(t, resp)
	if s.hits.Load() != 3 {
		t.Fatalf("second request took %d attempts, want 1", s.hits.Load()-2)
	}
}

// A server that keeps asking for a new nonce gets exactly one retry; the
// second refusal is returned.
func TestTransport_RetriesExactlyOnce(t *testing.T) {
	k := mustKey(t)
	s := newServer(t, k.Thumbprint())
	s.alwaysNonce = true
	c := &http.Client{Transport: &dpopclient.Transport{Signer: k, Token: token("tok")}}
	resp := do(t, c, "GET", s.URL+"/r", "")
	readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || s.hits.Load() != 2 {
		t.Fatalf("status %d after %d attempts, want 401 after 2", resp.StatusCode, s.hits.Load())
	}
}

// A body that cannot be replayed is not retried.
func TestTransport_UnreplayableBodyNotRetried(t *testing.T) {
	k := mustKey(t)
	s := newServer(t, k.Thumbprint())
	c := &http.Client{Transport: &dpopclient.Transport{Signer: k, Token: token("tok")}}
	req, _ := http.NewRequest("POST", s.URL+"/r", io.NopCloser(strings.NewReader("once")))
	req.GetBody = nil
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || s.hits.Load() != 1 {
		t.Fatalf("status %d after %d attempts, want 401 after 1", resp.StatusCode, s.hits.Load())
	}
	// The nonce was still learned: the next request succeeds first time.
	resp = do(t, c, "GET", s.URL+"/r", "")
	readAll(t, resp)
	if resp.StatusCode != 200 || s.hits.Load() != 2 {
		t.Fatalf("next request: status %d, attempts %d", resp.StatusCode, s.hits.Load())
	}
}

// Without a token the proof has no ath and no Authorization is sent (a mint
// endpoint).
func TestTransport_NoTokenNoATH(t *testing.T) {
	k := mustKey(t)
	s := newServer(t, "")
	var sawAuth atomic.Bool
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := &http.Client{Transport: &dpopclient.Transport{Base: base, Signer: k}}
	resp := do(t, c, "POST", s.URL+"/token", "")
	readAll(t, resp)
	if resp.StatusCode != 200 || sawAuth.Load() {
		t.Fatalf("status %d, Authorization sent: %v", resp.StatusCode, sawAuth.Load())
	}
}

// A nonce sent on a success (a first-attempt 200, no refusal involved) is
// used by the next proof.
func TestTransport_LearnsNonceFromSuccess(t *testing.T) {
	var hits atomic.Int32
	var nonces []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		var cl dpop.Claims
		_ = json.Unmarshal(payloadOf(t, r.Header.Get("DPoP")), &cl)
		mu.Lock()
		nonces = append(nonces, cl.Nonce)
		mu.Unlock()
		w.Header().Set("DPoP-Nonce", fmt.Sprintf("n-%d", n))
	}))
	defer srv.Close()
	c := &http.Client{Transport: &dpopclient.Transport{Signer: mustKey(t)}}
	for i := 0; i < 3; i++ {
		readAll(t, do(t, c, "GET", srv.URL+"/r", ""))
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"", "n-1", "n-2"}; strings.Join(nonces, ",") != strings.Join(want, ",") {
		t.Fatalf("proof nonces = %q, want %q", nonces, want)
	}
}

// The request the caller passed is not modified.
func TestTransport_DoesNotMutateRequest(t *testing.T) {
	k := mustKey(t)
	s := newServer(t, k.Thumbprint())
	tr := &dpopclient.Transport{Signer: k, Token: token("tok")}
	req, _ := http.NewRequest("GET", s.URL+"/r", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if req.Header.Get("DPoP") != "" || req.Header.Get("Authorization") != "" {
		t.Fatalf("caller's request was modified: %v", req.Header)
	}
}

func TestTransport_TokenErrorAndNoSigner(t *testing.T) {
	boom := errors.New("boom")
	tr := &dpopclient.Transport{Signer: mustKey(t), Token: func(context.Context) (string, error) { return "", boom }}
	req, _ := http.NewRequest("GET", "https://api.example.com/r", nil)
	if _, err := tr.RoundTrip(req); !errors.Is(err, boom) {
		t.Fatalf("token error = %v", err)
	}
	if _, err := (&dpopclient.Transport{}).RoundTrip(req); err == nil {
		t.Fatal("transport without signer succeeded")
	}
}

// Refusal recognises DEVICE_REVOKED / SESSION_EXPIRED in the error envelope
// and leaves the body readable.
func TestRefusal_HTTPEnvelope(t *testing.T) {
	mk := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString(body))}
	}
	body := `{"error":{"code":"UNAUTHENTICATED","message":"m","details":[{"field":"","code":"DEVICE_REVOKED","message":"x"}]}}`
	resp := mk(401, body)
	err := dpopclient.Refusal(resp)
	var re *dpopclient.RefusedError
	if !errors.As(err, &re) || !errors.Is(err, dpopclient.ErrDeviceRevoked) || errors.Is(err, dpopclient.ErrSessionExpired) || re.HTTPStatus != 401 {
		t.Fatalf("Refusal = %v", err)
	}
	if got := readAll(t, resp); got != body {
		t.Fatalf("body after Refusal = %q", got)
	}
	resp = mk(403, strings.Replace(body, "DEVICE_REVOKED", "SESSION_EXPIRED", 1))
	if err := dpopclient.Refusal(resp); !errors.Is(err, dpopclient.ErrSessionExpired) {
		t.Fatalf("403 SESSION_EXPIRED = %v", err)
	}
	for _, r := range []*http.Response{mk(401, `{"error":{"code":"UNAUTHENTICATED"}}`), mk(500, body), mk(401, "not json")} {
		if err := dpopclient.Refusal(r); err != nil {
			t.Fatalf("Refusal(%d) = %v, want nil", r.StatusCode, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
