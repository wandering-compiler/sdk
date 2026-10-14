// Package dpope2e signs the e2e runner's requests with device keys (DPoP,
// RFC 9449), for a generated suite whose surface binds tokens to devices.
//
// It is the runner's [runner.Signer]. The runner decides which device a step
// signs as (see runner/devices.go); this package holds each device's key and
// makes the proofs, through dpopclient.Transport for the net/http callers
// and the same nonce rule for the stress engine's client. It lives apart
// from the runner so that a suite whose surface does not bind devices never
// imports it, and so never depends on lib/dpop or JOSE.
//
//	signer := dpope2e.New()
//	rest := runner.NewRESTCaller(target, nil).WithSigner(signer)
//	streams := runner.NewSSEStreamCaller(target, nil).WithSigner(signer)
//	events := runner.NewSSESubscriber(target, nil).WithSigner(signer)
//	load := stress.NewLoadCaller(target, n, timeout).WithSigner(signer)
//
// A proof names the gateway's public origin (`<P>_DPOP_ORIGIN`). By default
// that is the origin of the caller's target; when the runner reaches the
// gateway by another address, [WithOrigin] names the public one: proofs
// carry it while requests still go to the target.
//
// Only a request that presents a token, or is a mint, to the caller's own
// origin (or to the WithOrigin public origin, which is dialled at the
// target) is signed. Anything else — an anonymous call, or a redirect to
// another origin (a presigned download URL) — goes out with no DPoP
// headers and no Authorization, as Go's client already drops Authorization
// on a redirect to another host.
//
// Keys are P-256, in memory, made on a device's first signed request and
// never written. A refusal carrying DEVICE_REVOKED marks the device
// revoked: it keeps its key, so its token keeps being refused for the
// right reason. Its next mint — signing in again — signs with a new key,
// which the device adopts only when that mint succeeds: a refused sign-in
// leaves the revoked key, and its token, as they were.
package dpope2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/go-jose/go-jose/v4"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// Signer is a [runner.Signer] signing with DPoP. One Signer may serve every
// caller of a suite; a device's key is shared by all of them.
type Signer struct {
	own    runner.Device // the device of a request made outside a step
	origin string        // the public origin proofs name; "" = the target's
	err    error         // a bad WithOrigin, reported by every signed request
}

var _ runner.Signer = (*Signer)(nil)

// Option configures a [Signer].
type Option func(*Signer)

// WithOrigin makes proofs name origin (scheme://host[:port]), the gateway's
// public origin (`<P>_DPOP_ORIGIN`), while requests still go to the
// caller's target: for a runner that reaches the gateway by an address
// other than the one the gateway is configured to see. The htu of a proof
// is origin followed by the request's path.
//
// A redirect to an absolute URL on origin is a redirect home: it is dialled
// at the target too, and signed. An origin with a path, query or fragment
// is refused: every signed request then fails, naming WithOrigin.
func WithOrigin(origin string) Option {
	return func(s *Signer) {
		u, err := url.Parse(origin)
		if s.origin = normOrigin(origin); s.origin == "" || err != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			s.origin = ""
			s.err = fmt.Errorf("dpope2e: WithOrigin(%q): want scheme://host[:port] with no path, query or fragment", origin)
		}
	}
}

// New returns a Signer.
func New(opts ...Option) *Signer {
	s := &Signer{}
	for _, o := range opts {
		o(s)
	}
	return s
}

// stateKey is where a device holds its [deviceState]. One key for the
// package: every Signer and every caller signs as a device with the same
// key.
type stateKey struct{}

// deviceState is one device's key, the signing transports built on it
// (one per client, each with its own nonce for its origin), the nonces
// [Signer.Do] learnt per origin, and whether the key was revoked.
type deviceState struct {
	mu         sync.Mutex
	key        *dpop.Key
	revoked    bool
	transports map[*signingRT]*dpopclient.Transport
	nonces     map[string]string
}

func stateOf(dv *runner.Device) *deviceState {
	return dv.Value(stateKey{}, func() any { return &deviceState{} }).(*deviceState)
}

// signingKey returns the key a request signs with: the device's (made when
// there is none), or — for a mint while that key is revoked — a candidate
// the device adopts only if the mint succeeds. Called with mu held.
func (st *deviceState) signingKey(mint bool) (key *dpop.Key, candidate bool, err error) {
	if st.key != nil && !(mint && st.revoked) {
		return st.key, false, nil
	}
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		return nil, false, err
	}
	if st.key == nil {
		st.key = k
		return k, false, nil
	}
	return k, true, nil
}

// transport returns the transport rt signs with for this request, its key,
// and whether the key is a candidate (its transport is then not kept).
func (st *deviceState) transport(rt *signingRT, mint bool) (*dpopclient.Transport, *dpop.Key, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	key, candidate, err := st.signingKey(mint)
	if err != nil {
		return nil, nil, false, err
	}
	if t, ok := st.transports[rt]; ok && !candidate {
		return t, key, false, nil
	}
	t, err := dpopclient.NewTransport(rt.redial, key, callToken, rt.signedOrigin())
	if err != nil {
		return nil, nil, false, err
	}
	if !candidate {
		if st.transports == nil {
			st.transports = map[*signingRT]*dpopclient.Transport{}
		}
		st.transports[rt] = t
	}
	return t, key, candidate, nil
}

// adopt makes a candidate key the device's after the mint signed with it
// succeeded; t (if any) is rt's transport for it.
func (st *deviceState) adopt(key *dpop.Key, rt *signingRT, t *dpopclient.Transport) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.revoked {
		return // another request adopted a key first
	}
	st.key, st.revoked = key, false
	st.transports, st.nonces = nil, nil
	if rt != nil && t != nil {
		st.transports = map[*signingRT]*dpopclient.Transport{rt: t}
	}
}

// markRevoked records that key was refused as revoked, if the device still
// signs with it.
func (st *deviceState) markRevoked(key *dpop.Key) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.key == key {
		st.revoked = true
	}
}

func success(status int) bool { return status >= 200 && status < 300 }

func callToken(ctx context.Context) (string, error) {
	tok, _ := runner.CallAuth(ctx)
	return tok, nil
}

// Client implements [runner.Signer].
func (s *Signer) Client(c *http.Client, baseURL string) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	base := cp.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rt := &signingRT{base: base, origin: normOrigin(baseURL), public: s.origin, err: s.err, own: &s.own}
	rt.redial = base
	if rt.public != "" {
		rt.redial = &redial{base: base, origin: rt.origin}
	}
	cp.Transport = rt
	return &cp
}

// signingRT signs each request as the device its context names (else the
// signer's own), for the token its context carries.
type signingRT struct {
	base   http.RoundTripper
	origin string            // the target's origin, where requests go
	public string            // the origin proofs name; "" = origin
	redial http.RoundTripper // sends a request on the public origin to the target
	err    error
	own    *runner.Device
}

func (rt *signingRT) signedOrigin() string {
	if rt.public != "" {
		return rt.public
	}
	return rt.origin
}

func (rt *signingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	o := originOf(req.URL)
	public := rt.public != "" && o == rt.public
	tok, mint := runner.CallAuth(req.Context())
	if tok == "" && !mint {
		if public {
			return rt.redial.RoundTrip(req)
		}
		return rt.base.RoundTrip(req)
	}
	fail := func(err error) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	switch {
	case rt.err != nil:
		return fail(rt.err)
	case rt.origin == "":
		return fail(dpopclient.ErrNoOrigins)
	case o != rt.origin && !public:
		// A redirect off the caller's origin (a presigned URL): the token
		// and the proofs stay home, as a Bearer does.
		return rt.base.RoundTrip(req)
	}
	dv := runner.DeviceFrom(req.Context())
	if dv == nil {
		dv = rt.own
	}
	st := stateOf(dv)
	t, key, candidate, err := st.transport(rt, mint)
	if err != nil {
		return fail(err)
	}
	if mint {
		req = dpopclient.WithMint(req)
	}
	if rt.public != "" && !public {
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host = splitOrigin(rt.public)
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	switch {
	case candidate && success(resp.StatusCode):
		st.adopt(key, rt, t)
	case !candidate && errors.Is(dpopclient.Refusal(resp), dpopclient.ErrDeviceRevoked):
		st.markRevoked(key)
	}
	return resp, nil
}

// redial sends a request addressed to the public origin to the target
// origin instead.
type redial struct {
	base   http.RoundTripper
	origin string
}

func (r *redial) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = splitOrigin(r.origin)
	out.Host = ""
	return r.base.RoundTrip(out)
}

// Do implements [runner.Signer]: the stress engine's requests, signed with
// dpopclient.Transport's rule — the last nonce the origin sent this key, and
// one retry when a 401 asks for a new one — and the same revocation rule as
// the callers. A request with no token that is not a mint goes out
// unsigned.
func (s *Signer) Do(dev *runner.Device, method, target, token string, mint bool, send func(http.Header) (int, http.Header, []byte, error)) (int, error) {
	if token == "" && !mint {
		status, _, _, err := send(http.Header{})
		return status, err
	}
	if s.err != nil {
		return -1, s.err
	}
	if dev == nil {
		dev = &s.own
	}
	st := stateOf(dev)
	u, err := url.Parse(target)
	if err != nil {
		return -1, err
	}
	htu := *u
	if s.origin != "" {
		htu.Scheme, htu.Host = splitOrigin(s.origin)
	}
	origin := originOf(&htu)

	st.mu.Lock()
	key, candidate, err := st.signingKey(mint)
	if err != nil {
		st.mu.Unlock()
		return -1, err
	}
	used := ""
	if !candidate {
		used = st.nonces[origin]
	}
	st.mu.Unlock()

	for attempt := 0; ; attempt++ {
		proof, err := dpop.NewProof(key, method, htu.String(), token, used)
		if err != nil {
			return -1, err
		}
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", dpop.AuthScheme+" "+token)
		}
		h.Set(dpop.HeaderProof, proof)
		status, resp, body, err := send(h)
		if err != nil {
			return status, err
		}
		fresh := resp.Get(dpop.HeaderNonce)
		if attempt == 0 && status == http.StatusUnauthorized && fresh != "" && fresh != used &&
			dpop.IsUseNonceChallenge(resp.Values(dpop.HeaderWWWAuthenticate)) {
			used = fresh
			continue
		}
		switch {
		case candidate && success(status):
			st.adopt(key, nil, nil)
		case !candidate:
			refusal := dpopclient.Refusal(&http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body))})
			if errors.Is(refusal, dpopclient.ErrDeviceRevoked) {
				st.markRevoked(key)
			}
		}
		if fresh != "" {
			st.mu.Lock()
			if st.key == key {
				if st.nonces == nil {
					st.nonces = map[string]string{}
				}
				st.nonces[origin] = fresh
			}
			st.mu.Unlock()
		}
		return status, nil
	}
}

// PublicJWK implements [runner.Signer]: the JSON of the device's public key
// as a JWK. The key is made when the device has none; a JWK of a key made
// here is never private.
func (s *Signer) PublicJWK(dev *runner.Device) (string, error) {
	if dev == nil {
		dev = &s.own
	}
	st := stateOf(dev)
	st.mu.Lock()
	key, _, err := st.signingKey(false)
	st.mu.Unlock()
	if err != nil {
		return "", err
	}
	jwk := jose.JSONWebKey{Key: key.Public()}
	if !jwk.IsPublic() {
		return "", errors.New("dpope2e: device key is not a public key")
	}
	b, err := jwk.MarshalJSON()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// originOf is the normalised scheme://host[:port] of u.
func originOf(u *url.URL) string {
	return normOrigin(u.Scheme + "://" + u.Host)
}

// normOrigin normalises an origin or base URL to scheme://host[:port] (lower
// case, no default port); "" when it has no scheme and host.
func normOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	o, err := dpop.NormalizeHTU(u.Scheme + "://" + u.Host)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(o, "/")
}

func splitOrigin(origin string) (scheme, host string) {
	scheme, host, _ = strings.Cut(origin, "://")
	return scheme, host
}
