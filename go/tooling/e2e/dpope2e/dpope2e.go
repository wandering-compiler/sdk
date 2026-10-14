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
// The target's origin must be the gateway's public origin (`<P>_DPOP_ORIGIN`):
// proofs name it, and requests to any other origin fail before they are
// sent.
//
// Keys are P-256, in memory, made on a device's first signed request and
// never written. A refusal carrying DEVICE_REVOKED discards the key it was
// for, so the device's next request — signing in again — makes a new one,
// as a client does after its device was revoked.
package dpope2e

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// Signer is a [runner.Signer] signing with DPoP. One Signer may serve every
// caller of a suite; a device's key is shared by all of them.
type Signer struct {
	own runner.Device // the device of a request made outside a step
}

var _ runner.Signer = (*Signer)(nil)

// New returns a Signer.
func New() *Signer { return &Signer{} }

// stateKey is where a device holds its [deviceState]. One key for the
// package: every Signer and every caller signs as a device with the same
// key.
type stateKey struct{}

// deviceState is one device's key, the signing transports built on it
// (one per client, each with its own nonce for its origin) and the nonces
// [Signer.Do] learnt, per origin.
type deviceState struct {
	mu         sync.Mutex
	key        *dpop.Key
	transports map[*signingRT]*dpopclient.Transport
	nonces     map[string]string
}

func stateOf(dv *runner.Device) *deviceState {
	return dv.Value(stateKey{}, func() any { return &deviceState{} }).(*deviceState)
}

// ensureKey makes the key when there is none. Called with mu held.
func (st *deviceState) ensureKey() error {
	if st.key != nil {
		return nil
	}
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		return err
	}
	st.key = k
	st.transports, st.nonces = nil, nil
	return nil
}

// transport returns the device's transport for rt and the key it signs
// with.
func (st *deviceState) transport(rt *signingRT) (*dpopclient.Transport, *dpop.Key, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.ensureKey(); err != nil {
		return nil, nil, err
	}
	if t, ok := st.transports[rt]; ok {
		return t, st.key, nil
	}
	t, err := dpopclient.NewTransport(rt.base, st.key, callToken, rt.origin)
	if err != nil {
		return nil, nil, err
	}
	if st.transports == nil {
		st.transports = map[*signingRT]*dpopclient.Transport{}
	}
	st.transports[rt] = t
	return t, st.key, nil
}

// discard drops key if the device still holds it.
func (st *deviceState) discard(key *dpop.Key) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.key == key {
		st.key, st.transports, st.nonces = nil, nil, nil
	}
}

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
	cp.Transport = &signingRT{base: base, origin: originOf(baseURL), own: &s.own}
	return &cp
}

// signingRT signs each request as the device its context names (else the
// signer's own), for the token its context carries.
type signingRT struct {
	base   http.RoundTripper
	origin string
	own    *runner.Device
}

func (rt *signingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	dv := runner.DeviceFrom(req.Context())
	if dv == nil {
		dv = rt.own
	}
	st := stateOf(dv)
	t, key, err := st.transport(rt)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if _, mint := runner.CallAuth(req.Context()); mint {
		req = dpopclient.WithMint(req)
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if errors.Is(dpopclient.Refusal(resp), dpopclient.ErrDeviceRevoked) {
		st.discard(key)
	}
	return resp, nil
}

// Do implements [runner.Signer]: the stress engine's requests, signed with
// dpopclient.Transport's rule — the last nonce the origin sent this key, and
// one retry when a 401 asks for a new one. A request with no token that is
// not a mint goes out unsigned.
func (s *Signer) Do(dev *runner.Device, method, target, token string, mint bool, send func(http.Header) (int, http.Header, error)) (int, error) {
	if token == "" && !mint {
		status, _, err := send(http.Header{})
		return status, err
	}
	if dev == nil {
		dev = &s.own
	}
	st := stateOf(dev)
	u, err := url.Parse(target)
	if err != nil {
		return -1, err
	}
	origin := u.Scheme + "://" + u.Host

	st.mu.Lock()
	if err := st.ensureKey(); err != nil {
		st.mu.Unlock()
		return -1, err
	}
	key, used := st.key, st.nonces[origin]
	st.mu.Unlock()

	for attempt := 0; ; attempt++ {
		proof, err := dpop.NewProof(key, method, target, token, used)
		if err != nil {
			return -1, err
		}
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", dpop.AuthScheme+" "+token)
		}
		h.Set(dpop.HeaderProof, proof)
		status, resp, err := send(h)
		if err != nil {
			return status, err
		}
		fresh := resp.Get(dpop.HeaderNonce)
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
		if attempt > 0 || status != http.StatusUnauthorized || fresh == "" || fresh == used ||
			!dpop.IsUseNonceChallenge(resp.Values(dpop.HeaderWWWAuthenticate)) {
			return status, nil
		}
		used = fresh
	}
}

// originOf is scheme://host[:port] of a base URL, "" when it has none (and
// then every signed request fails with dpopclient.ErrNoOrigins).
func originOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
