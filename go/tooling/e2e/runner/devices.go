package runner

import (
	"context"
	"net/http"
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
//     and signs an unbound proof with the device of the slot its token lands
//     in — the capture name its `expect` binds (`auth.token` by preference).
//     Signing in again into the same slot reuses that device, as one browser
//     signing in again does; a different slot (a second principal's
//     `bob.token`) is a different device. Step.Device names the device
//     explicitly, for a scenario about two devices of one principal.
//   - Every value the mint step captured is remembered as minted by that
//     device, so a later step presenting the token (`auth.token`,
//     `token_from`, an await's `token_from`, a `DPoP` credential) signs with
//     the key it is bound to, whatever capture it was copied into since.
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
	// the status and the response headers. Do signs for dev (nil: the
	// signer's own device) and calls send again, once, when the server asks
	// for a fresh nonce. It returns the last status.
	Do(dev *Device, method, target, token string, mint bool, send func(h http.Header) (int, http.Header, error)) (int, error)
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
	byToken map[string]*Device
}

// NewDevices returns an empty set.
func NewDevices() *Devices {
	return &Devices{byName: map[string]*Device{}, byToken: map[string]*Device{}}
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

// learn records what a successful mint step bound: each captured value is a
// credential that device minted, and each capture name now resolves to it.
func (d *Devices) learn(dv *Device, s Step, scope *runtime.Scope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range runtime.CaptureNames(s.Expect) {
		d.byName[name] = dv
		if v, ok := scope.Get(name); ok && v != nil {
			if tok := runtime.Text(v); tok != "" {
				d.byToken[tok] = dv
			}
		}
	}
}

// stepDevice picks the device a step signs with; presented is the token
// the step sends ("" for none).
func (d *Devices) stepDevice(s Step, presented string) *Device {
	switch {
	case s.Device != "":
		return d.named(s.Device)
	case s.Endpoint.Mint:
		return d.named(mintSlot(s))
	}
	return d.forToken(presented, s.TokenFrom)
}

// mintSlot is the capture a mint step's token lands in: `auth.token` when
// the step binds it, else the first bound name that mentions a token, else
// the first bound name, else `auth.token`.
func mintSlot(s Step) string {
	names := runtime.CaptureNames(s.Expect)
	for _, n := range names {
		if n == DefaultTokenSlot {
			return n
		}
	}
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), "token") {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return DefaultTokenSlot
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
