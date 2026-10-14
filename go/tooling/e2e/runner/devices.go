package runner

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// Device keys (DPoP, RFC 9449).
//
// Against a surface whose auth method binds tokens to device keys, every
// token is minted for the key that signed the sign-in's proof, and every
// later call presenting it must carry a proof signed by that same key. The
// runner models that the way a real client does:
//
//   - A run (one [RunSteps] call, so one scenario) has its own [Devices]: a
//     set of devices, each holding a key made on first use and never
//     written anywhere.
//   - A MINT step (Endpoint.Mint: sign-in, sign-up, …) sends no stored token
//     and signs an unbound proof. Its device is, in order: Step.Device when
//     set (two devices of one principal); the device of an UNFINISHED mint
//     one of its input values came from — a mint that captured no token,
//     such as SignIn answering mfa_required or SignUp awaiting confirmation
//     (VerifyMfa sends the challenge_id SignIn answered, and must be signed
//     by the key SignIn was); the device of
//     the slot its token lands in — the capture bound to the response's
//     `token` field, which every auth-plugin mint names so. Signing in again
//     into one slot reuses its device, as one browser signing in again
//     does; another principal's slot (`bob.token`) is another device. A
//     mint with none of these (a sign-up that captures no token, a refused
//     sign-in) signs as a device of its own, used by that step alone.
//   - The token a mint captured is remembered as that device's, so a later
//     step presenting it (`auth.token`, `token_from`, an await's
//     `token_from`, a `DPoP` credential) signs with the key it is bound to,
//     whatever capture it was copied into since. Every string an unfinished
//     mint captured is remembered too, for ONE later mint's input to chain
//     on (a challenge id is single-use); a finished mint's other captures
//     (a user id, a trust token) chain nothing, and no value but a token is
//     ever taken for a token.
//   - `${device_jwk:<name>}` in a step's input is the public JWK of the
//     device <name> (the `public_key` of CreateApiToken / IssueBotToken);
//     the token that step captures from its `token` field is that device's.
//
// This package chooses the device; it holds no key material. The key, the
// proofs and the nonces belong to a [Signer] — package
// tooling/e2e/dpope2e — which a caller takes with WithSigner. That keeps the
// JOSE dependency out of every suite whose surface does not bind devices:
// such a suite never imports dpope2e, and its callers send
// `Authorization: Bearer` exactly as before.

// DefaultTokenSlot is the capture a step's token is read from when it names
// no other: the scenario's session.
const DefaultTokenSlot = "auth.token"

// Signer signs a caller's requests with device keys. Package
// tooling/e2e/dpope2e implements it with DPoP.
type Signer interface {
	// Client returns a copy of c (nil: a default client) whose requests to
	// the origin of baseURL are signed by the device [DeviceFrom] names in
	// the request context (else the signer's own), for the token and mint
	// flag [CallAuth] reads from it. A request with neither a token nor a
	// mint flag goes out unsigned.
	Client(c *http.Client, baseURL string) *http.Client

	// Do is the same signing for a client that is not an http.Client (the
	// stress engine's): send applies h to the request, sends it and returns
	// the status, the response headers and — for a 401 or 403, so a
	// revocation can be read — the body. Do signs for dev (nil: the
	// signer's own device) and calls send again, once, when the server asks
	// for a fresh nonce. It returns the last status.
	Do(dev *Device, method, target, token string, mint bool, send func(h http.Header) (status int, resp http.Header, body []byte, err error)) (int, error)

	// PublicJWK is the JSON of dev's public key as a JWK (RFC 7517), with
	// no private member: what `${device_jwk:<name>}` expands to.
	PublicJWK(dev *Device) (string, error)
}

// signingCaller is a caller that may have a Signer.
type signingCaller interface {
	deviceSigner() Signer
}

// Device is one device: an identity a [Signer] keeps its key and nonces
// on. The zero value is ready to use.
type Device struct {
	mu    sync.Mutex
	state map[any]any
}

// Value returns the value the device holds under key, storing mk() there
// first when it holds none. A [Signer] keeps its per-device state with it.
func (d *Device) Value(key any, mk func() any) any {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.state[key]; ok {
		return v
	}
	if d.state == nil {
		d.state = map[any]any{}
	}
	v := mk()
	d.state[key] = v
	return v
}

// Devices is one run's devices. It is safe for concurrent use.
type Devices struct {
	mu      sync.Mutex
	byName  map[string]*Device
	byToken map[string]*Device // tokens a mint (or a device_jwk step) captured
	byChain map[string]*Device // strings an unfinished mint captured, until a mint uses one
}

// NewDevices returns an empty set.
func NewDevices() *Devices {
	return &Devices{byName: map[string]*Device{}, byToken: map[string]*Device{}, byChain: map[string]*Device{}}
}

type devicesKey struct{}

// WithDevices returns ctx carrying d. [RunSteps] uses the set it finds in
// its context, and makes a fresh one when there is none — so a caller that
// runs several step lists as one session (the stress engine's setup, load
// and teardown) passes one set to all of them.
func WithDevices(ctx context.Context, d *Devices) context.Context {
	return context.WithValue(ctx, devicesKey{}, d)
}

// DevicesFrom returns the set ctx carries, or nil.
func DevicesFrom(ctx context.Context) *Devices {
	d, _ := ctx.Value(devicesKey{}).(*Devices)
	return d
}

// named returns the device a name resolves to — the device a slot was last
// minted into, or the device of that name — making it on first use.
func (d *Devices) named(name string) *Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dv, ok := d.byName[name]; ok {
		return dv
	}
	dv := &Device{}
	d.byName[name] = dv
	return dv
}

// forToken returns the device that minted token, else the device of slot
// (the session slot when slot is empty).
func (d *Devices) forToken(token, slot string) *Device {
	if token != "" {
		d.mu.Lock()
		dv, ok := d.byToken[token]
		d.mu.Unlock()
		if ok {
			return dv
		}
	}
	if slot == "" {
		slot = DefaultTokenSlot
	}
	return d.named(slot)
}

// DeviceFor returns the device whose key token is bound to: the device that
// minted it, else the session slot's. The stress engine signs its load
// with it.
func (d *Devices) DeviceFor(token string) *Device {
	return d.forToken(token, DefaultTokenSlot)
}

// learn records what a successful mint step bound. The chain entries its
// input used are spent (a challenge id is single-use; a step that is
// refused leaves them for a retry). A finished mint — one that captured a
// token — binds that token to dv and its slot to dv; an unfinished one
// leaves every string it captured for the next mint to chain on.
func (d *Devices) learn(dv *Device, s Step, input map[string]any, scope *runtime.Scope) {
	d.mu.Lock()
	for _, str := range inputStrings(input, nil) {
		if d.byChain[str] == dv {
			delete(d.byChain, str)
		}
	}
	d.mu.Unlock()
	if tokenCapture(s.Expect) != "" {
		d.bindToken(dv, s, scope)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range runtime.CaptureNames(s.Expect) {
		if v, ok := scope.Get(name); ok {
			if str, ok := v.(string); ok && str != "" {
				d.byChain[str] = dv
			}
		}
	}
}

// inputStrings appends every string in input, in a stable order.
func inputStrings(input any, out []string) []string {
	switch v := input.(type) {
	case string:
		out = append(out, v)
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = inputStrings(v[k], out)
		}
	case []any:
		for _, e := range v {
			out = inputStrings(e, out)
		}
	}
	return out
}

// bindToken records the token a step captured from its `token` field as a
// credential of dv, and the capture's name as dv's slot.
func (d *Devices) bindToken(dv *Device, s Step, scope *runtime.Scope) {
	slot := tokenCapture(s.Expect)
	if slot == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byName[slot] = dv
	if v, ok := scope.Get(slot); ok && v != nil {
		if tok := runtime.Text(v); tok != "" {
			d.byToken[tok] = dv
		}
	}
}

// chained returns the device of an unfinished mint that captured one of
// the strings in input, or nil.
func (d *Devices) chained(input map[string]any) *Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, str := range inputStrings(input, nil) {
		if dv, ok := d.byChain[str]; ok {
			return dv
		}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stepDevice picks the device a step signs with; presented is the token
// the step sends ("" for none), input its expanded input.
func (d *Devices) stepDevice(s Step, presented string, input map[string]any) *Device {
	switch {
	case s.Device != "":
		return d.named(s.Device)
	case s.Endpoint.Mint:
		if dv := d.chained(input); dv != nil {
			return dv
		}
		if slot := tokenCapture(s.Expect); slot != "" {
			return d.named(slot)
		}
		return &Device{}
	}
	return d.forToken(presented, s.TokenFrom)
}

// tokenCapture is the capture a step binds its response's `token` field to,
// "" when it binds none.
func tokenCapture(expect map[string]any) string {
	m, ok := expect["token"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["capture"].(string)
	return name
}

type deviceKey struct{}

func withDevice(ctx context.Context, dv *Device) context.Context {
	return context.WithValue(ctx, deviceKey{}, dv)
}

// DeviceFrom returns the device the step running in ctx signs with, or nil
// outside a step.
func DeviceFrom(ctx context.Context) *Device {
	dv, _ := ctx.Value(deviceKey{}).(*Device)
	return dv
}

type callAuth struct {
	token string
	mint  bool
}

type callAuthKey struct{}

func withCallAuth(ctx context.Context, token string, mint bool) context.Context {
	return context.WithValue(ctx, callAuthKey{}, callAuth{token: token, mint: mint})
}

// CallAuth returns what a signing caller's request presents: its token (""
// for none) and whether it is a mint call.
func CallAuth(ctx context.Context) (token string, mint bool) {
	a, _ := ctx.Value(callAuthKey{}).(callAuth)
	return a.token, a.mint
}

// signedCredential splits a step credential of the DPoP scheme into its
// token: on a signing caller such a credential is a token like any other
// and is signed for. Any other credential (a Bearer the step means to send
// unsigned, an API-key scheme) is sent as written.
func signedCredential(credential string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(credential), " ")
	if !ok || !strings.EqualFold(scheme, "DPoP") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}
