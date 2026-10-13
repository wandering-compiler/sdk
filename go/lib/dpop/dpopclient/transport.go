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
	"fmt"
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
	// Origins are the origins (scheme://host[:port]) the token and proofs
	// may be sent to. Required: there is no implicit default. A request to
	// any other origin — a redirect to another host, for instance — fails
	// with [ErrOriginNotAllowed] before anything is sent or the token is
	// fetched.
	Origins []string

	mu      sync.Mutex
	nonces  map[string]string // origin → last nonce
	allowed map[string]bool
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// originOf is the normalised origin of a URL: lower-case scheme and host, no
// default port. "" when the URL has none.
func originOf(u *url.URL) string {
	o, err := dpop.NormalizeHTU(u.Scheme + "://" + u.Host)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(o, "/")
}

// ErrOriginNotAllowed is returned for a request to an origin outside
// [Transport.Origins], including a redirect target on another host.
var ErrOriginNotAllowed = errors.New("dpopclient: origin not in Transport.Origins")

// ErrNoOrigins is returned by a Transport whose Origins holds no valid origin.
var ErrNoOrigins = errors.New("dpopclient: Transport.Origins is empty or invalid")

// NewTransport returns a Transport that sends the token and proofs to the
// given origins only. It fails when origins holds no origin or an invalid
// one.
func NewTransport(base http.RoundTripper, signer dpop.Signer, token TokenFunc, origins ...string) (*Transport, error) {
	if signer == nil {
		return nil, errors.New("dpopclient: Transport needs a Signer")
	}
	if len(origins) == 0 {
		return nil, ErrNoOrigins
	}
	for _, o := range origins {
		if normOrigin(o) == "" {
			return nil, fmt.Errorf("%w: %q is not scheme://host[:port]", ErrNoOrigins, o)
		}
	}
	return &Transport{Base: base, Signer: signer, Token: token, Origins: origins}, nil
}

// normOrigin normalises a configured origin; "" when it is not a bare
// scheme://host[:port].
func normOrigin(o string) string {
	u, err := url.Parse(o)
	if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return originOf(u)
}

// mayCarry reports whether credentials may go to origin.
func (t *Transport) mayCarry(origin string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.allowed == nil {
		allowed := make(map[string]bool)
		for _, o := range t.Origins {
			if n := normOrigin(o); n != "" {
				allowed[n] = true
			}
		}
		if len(allowed) == 0 {
			return false, ErrNoOrigins
		}
		t.allowed = allowed
	}
	return origin != "" && t.allowed[origin], nil
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
	origin := originOf(req.URL)
	switch ok, err := t.mayCarry(origin); {
	case err != nil:
		closeBody()
		return nil, err
	case !ok:
		closeBody()
		return nil, fmt.Errorf("%w: %s", ErrOriginNotAllowed, origin)
	}
	token := ""
	if t.Token != nil {
		var err error
		if token, err = t.Token(req.Context()); err != nil {
			closeBody()
			return nil, err
		}
	}
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
