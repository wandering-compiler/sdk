// Package dpopclient attaches DPoP proofs (package dpop) to outgoing HTTP
// requests and gRPC calls, follows the server's nonces, and retries once when
// the server asks for a fresh one.
//
// HTTP: [Transport] is an http.RoundTripper. gRPC: [Credentials] is a
// credentials.PerRPCCredentials plus the client interceptors that read the
// nonce from response metadata and retry; [Credentials.DialOptions] installs
// all three.
//
// Both remember the last nonce the server sent — on any response, success or
// refusal — and put it in the next proof. A refusal carrying
// `use_dpop_nonce` and a new nonce is retried exactly once with a new proof;
// a second refusal is returned to the caller.
package dpopclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// TokenFunc returns the access token for a request, or "" for a request that
// carries none (a mint endpoint; its proof has no ath).
type TokenFunc func(ctx context.Context) (string, error)

// Transport is an http.RoundTripper that adds `Authorization: DPoP <token>`
// (when Token returns one) and a fresh `DPoP` proof to every request.
//
// The htu of each proof is the request URL without query and fragment, so the
// client must address the server by the public origin the server is
// configured with.
//
// A request whose body cannot be replayed (Body set, GetBody nil) is not
// retried; its 401 is returned as is. Requests built by http.NewRequest from
// a bytes or strings reader have GetBody.
type Transport struct {
	// Base performs the requests; nil means http.DefaultTransport.
	Base http.RoundTripper
	// Signer holds the device key. Required.
	Signer dpop.Signer
	// Token supplies the access token; nil means no token.
	Token TokenFunc

	mu     sync.Mutex
	nonces map[string]string // origin → last nonce
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

func originOf(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func (t *Transport) nonce(origin string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nonces[origin]
}

// learn stores a nonce the server sent and reports whether it differs from
// the one stored before.
func (t *Transport) learn(origin, nonce string) bool {
	if nonce == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nonces == nil {
		t.nonces = make(map[string]string)
	}
	changed := t.nonces[origin] != nonce
	t.nonces[origin] = nonce
	return changed
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	closeBody := func() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
	}
	if t.Signer == nil {
		closeBody()
		return nil, errors.New("dpopclient: Transport has no Signer")
	}
	token := ""
	if t.Token != nil {
		var err error
		if token, err = t.Token(req.Context()); err != nil {
			closeBody()
			return nil, err
		}
	}
	origin := originOf(req.URL)
	used := t.nonce(origin)
	out, err := t.prepare(req, req.Body, token, used)
	if err != nil {
		closeBody()
		return nil, err
	}
	resp, err := t.base().RoundTrip(out)
	if err != nil {
		return nil, err
	}
	fresh := resp.Header.Get(dpop.HeaderNonce)
	t.learn(origin, fresh)
	if resp.StatusCode != http.StatusUnauthorized || fresh == "" || fresh == used ||
		!dpop.IsUseNonceChallenge(resp.Header.Values(dpop.HeaderWWWAuthenticate)) {
		return resp, nil
	}
	// Retry once with the new nonce, if the body can be sent again.
	var body io.ReadCloser
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, nil
		}
		if body, err = req.GetBody(); err != nil {
			return resp, nil
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	retry, err := t.prepare(req, body, token, fresh)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, err
	}
	resp, err = t.base().RoundTrip(retry)
	if err != nil {
		return nil, err
	}
	t.learn(origin, resp.Header.Get(dpop.HeaderNonce))
	return resp, nil
}

// prepare clones req with body and the DPoP headers for one attempt.
func (t *Transport) prepare(req *http.Request, body io.ReadCloser, token, nonce string) (*http.Request, error) {
	proof, err := dpop.NewProof(t.Signer, req.Method, req.URL.String(), token, nonce)
	if err != nil {
		return nil, err
	}
	out := req.Clone(req.Context())
	out.Body = body
	if token != "" {
		out.Header.Set("Authorization", dpop.AuthScheme+" "+token)
	}
	out.Header.Set(dpop.HeaderProof, proof)
	return out, nil
}
