package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// runCfg holds run-scoped dependencies threaded from RunScenario/RunSteps
// into each step. Kept separate from the per-step Step so the generated
// caller wires it once (via RunOption) rather than baking it into every
// literal.
type runCfg struct {
	events  EventSubscriber
	streams StreamCaller
}

// RunOption configures a run. Options are variadic on RunScenario/RunSteps
// so existing callers (and the stress engine) compile unchanged.
type RunOption func(*runCfg)

// WithEventSubscriber supplies the subscriber a step's AwaitEvent uses to
// tap the gateway's `/w17-events` stream. Without it, a step that awaits an
// event fails with a clear "no event subscriber configured" error.
func WithEventSubscriber(s EventSubscriber) RunOption {
	return func(c *runCfg) { c.events = s }
}

// WithStreamCaller supplies the caller a step's ExpectStream uses to open a
// streaming ENDPOINT. Without it, a step that declares `expect_stream` fails
// with a clear error rather than skipping the assertion — a stream case that
// quietly did nothing would be worse than no case at all, which is the state
// this feature replaced.
func WithStreamCaller(sc StreamCaller) RunOption {
	return func(c *runCfg) { c.streams = sc }
}

// format selects per-step output: "text" (the default ✓/✗ checklist) or
// "json" (one NDJSON record per step for machine consumption). The
// generated runner sets it from its --format flag.
var format = "text"

// SetFormat selects the per-step output format ("text" or "json").
// Unknown values fall back to text.
func SetFormat(f string) {
	if f == "json" {
		format = "json"
		return
	}
	format = "text"
}

// stepRecord is the JSON shape emitted per step in --format=json. A pass
// carries the routing fields; a fail adds the full error text (the step's
// formatted failure — HTTP status + body, or the matcher mismatch).
type stepRecord struct {
	Type      string `json:"type"` // always "result"
	Status    string `json:"status"`
	Step      int    `json:"step"`
	Total     int    `json:"total"`
	Label     string `json:"label"`
	Ref       string `json:"ref"`
	Transport string `json:"transport"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Step is one baked call in a scenario: an endpoint (routing) plus the
// request contract + the response contract. The codegen emits these as
// Go literals (from the YAML, at build time) — there is no runtime YAML.
type Step struct {
	// Endpoint is the routing record (transport + REST route / MCP tool
	// + auth flag).
	Endpoint Endpoint

	// Input is the request contract: field → value, generator
	// (`${random:…}`, `${seq}`) or capture ref (`${name}`).
	Input map[string]any

	// Expect is the response contract: field → matcher.
	Expect map[string]any

	// Headers are static request headers (REST), values interpolated
	// like Input (`${seq}` / `${name}`). Empty = none.
	Headers map[string]string

	// Credential, when set, is the whole `Authorization` header value for
	// this step and SUPPRESSES the reserved-token injection. See
	// e2espec.Doc.Credential for why `Headers` cannot carry it.
	Credential string

	// Anonymous sends this step's call with NO credential at all, even to
	// an auth-required endpoint and even after the scenario captured
	// `auth.token`. It is how a step asserts that a signed-out caller is
	// refused: without it, every auth-required step either carries the
	// session token or fails before sending anything. Exclusive with
	// Credential and TokenFrom.
	Anonymous bool

	// TokenFrom names the capture whose value is this step's bearer token,
	// in place of the reserved `auth.token` — a second signed-in principal
	// in the same scenario, or the session token named on purpose (a step
	// asserting that this very session is now refused, after a sign-out).
	// The capture must be bound and non-empty. Exclusive with Credential
	// and Anonymous.
	TokenFrom string

	// Device names the device key this step signs with on a caller built
	// WithSigner, in place of the one its token implies (see devices.go). Two
	// sign-ins of one principal with different Device values are two
	// devices; a step presenting one device's token under another Device
	// asserts the refusal of a proof by the wrong key. Empty: derived.
	// Ignored by callers that do not sign.
	Device string

	// Repeat runs this step N times sequentially (the `${seq}`
	// generator advances each iteration). Zero/one = once.
	Repeat int

	// Label is a human tag for failure messages (the source test file).
	Label string

	// ExpectError, when set, asserts that the call is REFUSED and how.
	// Mutually exclusive with Expect (the spec parser rejects both).
	ExpectError *ExpectError

	// ExpectTransportError, when set, asserts a refusal the SURFACE never
	// answered — one with no canonical code. Mutually exclusive with the
	// other two (the spec parser rejects the combination).
	ExpectTransportError *ExpectTransportError

	// ExpectStream, when set, makes this step OPEN the endpoint's own
	// stream and assert the sequence of frames it sends, rather than issue
	// a unary call. Mutually exclusive with Expect (the spec parser rejects
	// both); it pairs with ExpectError, which then asserts a refused
	// connect.
	ExpectStream *ExpectStream

	// AwaitEvents, when non-empty, asserts that each listed public event
	// lands on the gateway's `/w17-events` SSE stream after this step's
	// call. The runner subscribes to every one BEFORE the call (the stream
	// has no replay), issues the call, then waits + matches each payload. A
	// step routinely awaits a single event; a method that emits several
	// public events lists them all.
	AwaitEvents []AwaitEvent

	// Files are multipart file parts sent with this step's call. When
	// non-empty the REST caller switches the request to
	// `multipart/form-data` — see FilePart.
	Files []FilePart
}

// RunScenario executes one scenario — an ordered, flattened step
// sequence (a domain×transport suite, with any pre/post actions already
// inlined by the codegen). All steps share ONE capture scope, so state
// flows across them (a later step can use an id an earlier step
// captured). It is a dependency chain: the first failed step aborts the
// scenario and is returned. The generated test func wraps the result in
// t.Fatal, so the `testing` framework owns reporting + exit code; this
// stays a plain library function (no testing.TB — that can't be faked,
// and fail-fast suits a dependency chain).
func RunScenario(ctx context.Context, steps []Step, callers map[string]Caller, opts ...RunOption) error {
	return RunSteps(ctx, runtime.NewRun().NewScope(), steps, callers, opts...)
}

// RunSteps runs an ordered step sequence against a CALLER-PROVIDED scope,
// so the caller keeps access to the captures the run bound (the stress
// engine runs its once-only setup phase through here, then reads
// `auth.token` + any captured ids off the scope to seed its load workers).
// Same fail-fast dependency-chain semantics as RunScenario, which is now a
// thin wrapper that supplies a fresh scope.
func RunSteps(ctx context.Context, scope *runtime.Scope, steps []Step, callers map[string]Caller, opts ...RunOption) error {
	var cfg runCfg
	for _, o := range opts {
		o(&cfg)
	}
	if DevicesFrom(ctx) == nil {
		ctx = WithDevices(ctx, NewDevices())
	}
	total := 0
	for _, s := range steps {
		if s.Repeat > 1 {
			total += s.Repeat
		} else {
			total++
		}
	}

	done := 0
	for i, s := range steps {
		reps := s.Repeat
		if reps < 1 {
			reps = 1
		}
		for r := 0; r < reps; r++ {
			done++
			if err := runStep(ctx, scope, s, callers, cfg); err != nil {
				var wrapped error
				if reps > 1 {
					wrapped = fmt.Errorf("step %d %s [%s] iter %d/%d: %w", i+1, s.Label, s.Endpoint.Ref, r+1, reps, err)
				} else {
					wrapped = fmt.Errorf("step %d %s [%s]: %w", i+1, s.Label, s.Endpoint.Ref, err)
				}
				emitStep(done, total, "fail", s, wrapped.Error())
				return wrapped
			}
			emitStep(done, total, "pass", s, "")
		}
	}
	return nil
}

// expandHeaders interpolates each header value through the scope (same
// `${seq}` / `${name}` substitution as Input). Returns nil for an empty
// map so callers can cheaply skip header-setting.
func expandHeaders(h map[string]string, scope *runtime.Scope) (map[string]string, error) {
	if len(h) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		ev, err := runtime.Expand(v, scope)
		if err != nil {
			return nil, err
		}
		out[k] = fmt.Sprint(ev)
	}
	return out, nil
}

// expandOne interpolates a single value through the scope, the same way
// expandHeaders does each of its own. Separate rather than reusing that
// helper through a one-entry map: the two carry different meanings and a
// map is not the shape a single credential has.
func expandOne(v string, scope *runtime.Scope) (string, error) {
	if v == "" {
		return "", nil
	}
	ev, err := runtime.Expand(v, scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(ev), nil
}

// emitStep reports one step's outcome to stdout, in the selected format.
// text → the ✓/✗ checklist line (a passing run reads as a clean
// checklist of every call the scenario made, pre/post chain included, in
// execution order). json → one NDJSON `stepRecord` per step.
func emitStep(n, total int, status string, s Step, errMsg string) {
	if format == "json" {
		rec := stepRecord{
			Type: "result", Status: status, Step: n, Total: total,
			Label: s.Label, Ref: s.Endpoint.Ref, Transport: s.Endpoint.Transport,
			Method: s.Endpoint.HTTPMethod, Path: s.Endpoint.PathTemplate, Error: errMsg,
		}
		b, _ := json.Marshal(rec)
		fmt.Fprintln(os.Stdout, string(b))
		return
	}
	mark := "✓"
	if status == "fail" {
		mark = "✗"
	}
	route := s.Endpoint.Ref
	if s.Endpoint.HTTPMethod != "" {
		route = s.Endpoint.HTTPMethod + " " + s.Endpoint.PathTemplate
	}
	fmt.Fprintf(os.Stdout, "  %s [%2d/%2d] %-34s %s\n", mark, n, total, s.Label, route)
}

// runStep expands the input, dials the endpoint via its transport's
// caller (injecting the captured auth token when the endpoint needs
// one), and asserts the response — which also binds this step's
// captures into the shared scope.
func runStep(ctx context.Context, scope *runtime.Scope, s Step, callers map[string]Caller, cfg runCfg) error {
	caller, ok := callers[s.Endpoint.Transport]
	if !ok {
		return fmt.Errorf("no caller for transport %q", s.Endpoint.Transport)
	}
	devs := DevicesFrom(ctx)
	if devs == nil {
		devs = NewDevices()
		ctx = WithDevices(ctx, devs)
	}
	// `${device_jwk:<device>}`: the device's public key, which only the
	// signer of a signing caller can give. The token the step then captures
	// is bound to that device's key.
	var keyDevice *Device
	deviceJWK := func(name string) (any, error) {
		var sg Signer
		if sc, ok := caller.(signingCaller); ok {
			sg = sc.deviceSigner()
		}
		if sg == nil {
			return nil, fmt.Errorf("${device_jwk:%s}: the %q caller does not sign (no WithSigner), so there is no device key", name, s.Endpoint.Transport)
		}
		if name == "" {
			return nil, fmt.Errorf("${device_jwk:}: name the device")
		}
		if keyDevice != nil && keyDevice != devs.named(name) {
			return nil, fmt.Errorf("${device_jwk:%s}: a step binds its token to one device's key", name)
		}
		keyDevice = devs.named(name)
		return sg.PublicJWK(keyDevice)
	}
	expanded, err := runtime.ExpandWith(s.Input, scope, map[string]func(string) (any, error){runtime.DeviceJWKPrefix: deviceJWK})
	if err != nil {
		return fmt.Errorf("expand input: %w", err)
	}
	input, _ := expanded.(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	token, err := stepToken(s, scope)
	if err != nil {
		return err
	}
	headers, err := expandHeaders(s.Headers, scope)
	if err != nil {
		return fmt.Errorf("expand headers: %w", err)
	}
	credential, err := expandOne(s.Credential, scope)
	if err != nil {
		return fmt.Errorf("expand credential: %w", err)
	}

	// The device key this step signs with, on callers that sign. The
	// credential of the DPoP scheme is a token too, and its key is the one
	// that minted it.
	presented := token
	if tok, ok := signedCredential(credential); ok {
		presented = tok
	}
	dev := devs.stepDevice(s, presented, input)
	ctx = withDevice(ctx, dev)

	// Open every event subscription BEFORE the call so an async event the
	// call triggers can't land before we're listening (the stream has no
	// replay). One subscription per awaited topic keeps each Await from
	// consuming a sibling's frame. Subscriptions are live once Subscribe
	// returns.
	subs, err := subscribeAwaits(ctx, s, cfg, scope, token, credential, headers)
	defer func() {
		for _, sub := range subs {
			_ = sub.Close()
		}
	}()
	if err != nil {
		return err
	}

	// Everything a subscription delivered up to now predates this call, so
	// none of it can be this call's event: an earlier step's event that
	// arrived late is exactly such a frame.
	sent := time.Now()
	for _, sub := range subs {
		if st, ok := sub.(Settable); ok {
			st.IgnoreBefore(sent)
		}
	}

	// A streaming endpoint is OPENED, not called: its response is the frame
	// sequence, and there is no single body to match. EVERY step on one takes
	// this path, whatever it asserts — a stream step sent down the unary
	// caller would read a 200 with an event-stream body as a success, and that
	// is a green over an endpoint that may have answered nothing but an error.
	if s.Endpoint.Stream || s.ExpectStream != nil {
		if cfg.streams == nil {
			return fmt.Errorf("step opens the stream %s but no stream caller configured (pass runner.WithStreamCaller)", s.Endpoint.Ref)
		}
		if err := runStreamStep(ctx, s, cfg.streams, input, token, headers, scope); err != nil {
			return err
		}
	} else {
		if err := runUnaryStep(ctx, s, caller, input, token, headers, credential, scope); err != nil {
			return err
		}
	}
	switch {
	case keyDevice != nil:
		devs.bindToken(keyDevice, s, scope)
	case s.Endpoint.Mint:
		devs.learn(dev, s, input, scope)
	}

	for i, ae := range s.AwaitEvents {
		if err := awaitOne(ctx, ae, subs[i], scope); err != nil {
			return err
		}
	}
	return nil
}

// subscribeAwaits opens one subscription per awaited event, each as the
// principal that await observes: the await's own TokenFrom when it names
// one, else the step's — its Credential, its token, or (for a step whose
// own endpoint needs none) the scenario's session.
//
// The step's headers ride the subscribe too, unless the await names its
// own. The stream is authenticated by the same auth method as the call, and
// that method reads client-chosen headers: `W17-Org` picks the active org,
// whose id is the label the /w17-events hub partitions by. Subscribing
// without them would connect a label-less principal — which on an isolating
// surface is entitled to NO event — so the await would time out on a
// surface that works.
func subscribeAwaits(ctx context.Context, s Step, cfg runCfg, scope *runtime.Scope, token, credential string, headers map[string]string) ([]Subscription, error) {
	if len(s.AwaitEvents) == 0 {
		return nil, nil
	}
	if cfg.events == nil {
		return nil, fmt.Errorf("step awaits event %q but no event subscriber configured (pass runner.WithEventSubscriber)", s.AwaitEvents[0].Topic)
	}
	var subs []Subscription
	for _, ae := range s.AwaitEvents {
		if ae.Absent && bindsCapture(ae.Match) {
			return subs, fmt.Errorf("await_event %q: an absent event has nothing to capture from — drop the capture from its match", ae.Topic)
		}
		subHeaders := headers
		if ae.Headers != nil {
			h, err := expandHeaders(ae.Headers, scope)
			if err != nil {
				return subs, fmt.Errorf("await_event %q: expand headers: %w", ae.Topic, err)
			}
			subHeaders = h
		}
		var sub Subscription
		var err error
		switch {
		case ae.TokenFrom != "":
			tok, terr := capturedToken(scope, ae.TokenFrom)
			if terr != nil {
				return subs, fmt.Errorf("await_event %q: %w", ae.Topic, terr)
			}
			// Another principal's stream signs with that principal's key.
			actx := ctx
			if devs := DevicesFrom(ctx); devs != nil {
				actx = withDevice(ctx, devs.forToken(tok, ae.TokenFrom))
			}
			sub, err = cfg.events.Subscribe(actx, ae.Path, []string{ae.Topic}, tok, subHeaders)
		case credential != "":
			// The step's own credential, not the session's: the stream has to
			// be that principal's for the await to say anything about what
			// that principal is shown.
			cs, ok := cfg.events.(CredentialSubscriber)
			if !ok {
				return subs, fmt.Errorf("await_event %q: the step presents its own credential, and the event subscriber cannot present one (it does not implement runner.CredentialSubscriber) — subscribing as anyone else would observe the wrong principal", ae.Topic)
			}
			sub, err = cs.SubscribeWithCredential(ctx, ae.Path, []string{ae.Topic}, credential, subHeaders)
		default:
			evToken := token
			actx := ctx
			if evToken == "" {
				// The /w17-events stream is gated by the same realm as REST;
				// reuse the scenario's captured token even when this step's own
				// endpoint needs none — signed by the key that minted it.
				if v, ok := scope.Get(DefaultTokenSlot); ok && v != nil {
					evToken = runtime.Text(v)
				}
				if devs := DevicesFrom(ctx); devs != nil && evToken != "" {
					actx = withDevice(ctx, devs.forToken(evToken, DefaultTokenSlot))
				}
			}
			sub, err = cfg.events.Subscribe(actx, ae.Path, []string{ae.Topic}, evToken, subHeaders)
		}
		if err != nil {
			return subs, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// awaitOne asserts one awaited event after the step's call.
//
// Events on the topic whose payload does not match are skipped, not failed:
// the topic alone does not say which call an event came from, and the first
// one to arrive may be an earlier step's. The match runs on a fork of the
// scope, so a skipped event binds nothing; the matching one's captures are
// kept.
func awaitOne(ctx context.Context, ae AwaitEvent, sub Subscription, scope *runtime.Scope) error {
	timeout := time.Duration(ae.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = DefaultAwaitTimeoutMs * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var skipped int
	var lastMismatch error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return awaitTimedOut(ae, timeout, skipped, lastMismatch)
		}
		ev, err := sub.Await(ctx, ae.Topic, remaining)
		if err != nil {
			if timedOut(ctx, err, deadline) && (ae.Absent || skipped > 0) {
				return awaitTimedOut(ae, timeout, skipped, lastMismatch)
			}
			if ae.Absent {
				return fmt.Errorf("await_event %q (absent): the stream failed before the window closed, so it proves nothing about delivery: %w", ae.Topic, err)
			}
			return fmt.Errorf("await_event %q: %w", ae.Topic, err)
		}
		trial := scope.Fork()
		if err := runtime.MatchExpect(ae.Match, ev.Data, trial); err != nil {
			skipped++
			lastMismatch = err
			continue
		}
		if ae.Absent {
			return fmt.Errorf("await_event %q (absent): the event WAS delivered within %s — payload %s", ae.Topic, timeout, ev.Raw)
		}
		scope.Adopt(trial)
		return nil
	}
}

// awaitTimedOut reports a window that closed: success for an absent event,
// otherwise a failure that says whether nothing arrived or only events this
// step's match did not accept.
func awaitTimedOut(ae AwaitEvent, timeout time.Duration, skipped int, lastMismatch error) error {
	switch {
	case ae.Absent:
		return nil
	case skipped == 0:
		return fmt.Errorf("await_event %q: %w after %s waiting for event on topic %q", ae.Topic, ErrAwaitTimeout, timeout, ae.Topic)
	default:
		return fmt.Errorf("await_event %q payload: %d event(s) on the topic arrived within %s and none matched; the last: %w", ae.Topic, skipped, timeout, lastMismatch)
	}
}

// timedOut reports an Await that ended because the window closed: the
// subscription said so, or the deadline has passed while the run's own
// context is still live (a subscription that words its timeout otherwise).
func timedOut(ctx context.Context, err error, deadline time.Time) bool {
	if errors.Is(err, ErrAwaitTimeout) {
		return true
	}
	return ctx.Err() == nil && !time.Now().Before(deadline)
}

// bindsCapture reports whether a match binds a capture anywhere in it.
func bindsCapture(match map[string]any) bool {
	return runtime.BindsCapture(match)
}

// stepToken resolves the bearer token a step presents ("" for none).
//
// A capability-link endpoint IS auth-required, but its credential rides in
// the URL. Injecting the scenario's bearer token there would be worse than
// redundant: the case would pass while exercising a request no recipient of
// such a link can make, and it would fail outright in any scenario that
// never signed anyone in — which is the only honest way to write that case.
//
// A step naming its own credential does not take the reserved token either:
// presenting two is a request no caller makes, and leaving the bearer on
// would let the scenario's session answer for the credential under test.
func stepToken(s Step, scope *runtime.Scope) (string, error) {
	switch {
	case s.Anonymous && (s.Credential != "" || s.TokenFrom != ""):
		return "", fmt.Errorf("step is anonymous and also names a credential or token_from — an anonymous call presents nothing, so the two contradict each other")
	case s.Credential != "" && s.TokenFrom != "":
		return "", fmt.Errorf("step names both a credential and token_from %q — a call carries one Authorization value", s.TokenFrom)
	case s.Anonymous || s.Credential != "":
		return "", nil
	case s.Endpoint.Mint && s.TokenFrom != "":
		return "", fmt.Errorf("step names token_from %q on the mint %s — a mint call presents no token (the gateway checks a presented token before the sign-in runs)", s.TokenFrom, s.Endpoint.Ref)
	case s.Endpoint.Mint:
		// A token mint sends no stored token, even to an endpoint that
		// requires auth: the edge checks a presented token before the route
		// runs, so a stale session would fail signing in again.
		return "", nil
	case s.TokenFrom != "":
		return capturedToken(scope, s.TokenFrom)
	case !s.Endpoint.AuthRequired || s.Endpoint.CredentialInURL:
		return "", nil
	}
	if s.ExpectError != nil && s.ExpectError.Code == "UNAUTHENTICATED" {
		// The session token rides this call only because nobody said
		// otherwise, and the step expects to be told it is not signed in.
		// A refusal here cannot show what the step is named for: a server
		// that turned away EVERY signed-in call would pass it. Say which
		// of the two cases is meant.
		return "", fmt.Errorf("step expects UNAUTHENTICATED but would present the scenario's session token (auth.token) by default — " +
			"a refusal of that call does not show that a signed-out caller is refused. " +
			"Mark the step anonymous (`anonymous: true`) to send it with no credential, or name the token " +
			"(`token_from: auth.token`) if the point is that this session itself is refused, e.g. after a sign-out")
	}
	tok, err := capturedToken(scope, "auth.token")
	if err != nil {
		return "", fmt.Errorf("auth-required endpoint but %w", err)
	}
	return tok, nil
}

// capturedToken reads a token capture. A bound-but-empty value is an error
// rather than a token: interpolating it would send `Bearer ` or
// `Bearer <nil>`, and every refusal the scenario then asserts would pass on
// a sign-in that never produced a token.
func capturedToken(scope *runtime.Scope, name string) (string, error) {
	v, ok := scope.Get(name)
	if !ok {
		return "", fmt.Errorf("no %s captured upstream in this scenario", name)
	}
	tok := ""
	if v != nil {
		tok = runtime.Text(v)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("%s was captured but is empty (%v) — the step that bound it did not receive a token", name, v)
	}
	return tok, nil
}

// runUnaryStep issues a non-streaming call and asserts its one outcome: the
// response, a coded refusal, or a refusal below the surface.
func runUnaryStep(ctx context.Context, s Step, caller Caller, input map[string]any, token string, headers map[string]string, credential string, scope *runtime.Scope) error {
	resp, err := caller.Call(ctx, s.Endpoint, input, token, headers, credential, s.Files)
	switch {
	case s.ExpectTransportError != nil:
		return matchTransportError(s.ExpectTransportError, err, scope)
	case s.ExpectError != nil:
		return matchCallError(s.ExpectError, err, scope)
	case err != nil:
		return err
	default:
		return runtime.MatchExpect(s.Expect, resp, scope)
	}
}

// ExpectError is a step's refusal contract — the canonical code the
// surface must answer with, and optionally a matcher over its message.
// FilePart is one multipart file part of a step's call: the form part
// name (the REST registry's `http_name`), the declared filename, and the
// body.
//
// The part NAME is the binding under test. The registry says which part
// fills which message field; a step that addressed the FIELD would pass
// even with the registry's http_name wrong, which is exactly how a
// multipart endpoint breaks without anyone noticing.
type FilePart struct {
	Part     string
	Filename string
	Content  string
}

type ExpectError struct {
	Code    string
	Message any

	// Details, when non-empty, additionally asserts entries of the
	// refusal's per-field detail list. Each listed entry must be
	// PRESENT among the returned details; extras are allowed, because a
	// single request routinely trips more than one rule and pinning the
	// full set would make every case depend on the others' fixtures.
	Details []ExpectErrorDetail

	// TimeoutMs bounds the wait for a STREAM's refusal frame. Zero → the
	// declared frames' timeout, else DefaultAwaitTimeoutMs. A unary call
	// ignores it; codegen refuses it there.
	TimeoutMs int
}

// ExpectErrorDetail is one expected `details[]` entry. Only the set
// fields are asserted — a case that cares which RULE fired names the
// code, one that cares about the prose names the message.
type ExpectErrorDetail struct {
	Field   string
	Code    string
	Message any
}

// matchCallError asserts that the call was refused, and refused for the
// declared reason.
//
// A call that SUCCEEDED is the interesting failure here and gets its own
// message: for an authorization step it means the guard let the caller
// through, which is the leak the step exists to catch, and "expected an
// error, got none" buries that.
func matchCallError(want *ExpectError, err error, scope *runtime.Scope) error {
	if err == nil {
		return fmt.Errorf("expected the call to be refused with %s, but it SUCCEEDED — the guard under test let this caller through", want.Code)
	}
	var ce *CallError
	if !errors.As(err, &ce) {
		// A transport-level failure (connection refused, timeout) is not a
		// refusal, and must not be mistaken for one.
		return fmt.Errorf("expected a refusal carrying %s, but the call failed before the surface answered: %w", want.Code, err)
	}
	if ce.Code != want.Code {
		if ce.Code == "" {
			return fmt.Errorf("expected %s, got status %d with no canonical code in the body: %s", want.Code, ce.Status, ce.Raw)
		}
		return fmt.Errorf("expected %s, got %s (status %d): %s", want.Code, ce.Code, ce.Status, ce.Message)
	}
	if want.Message != nil {
		if err := runtime.MatchExpect(map[string]any{"message": want.Message}, map[string]any{"message": ce.Message}, scope); err != nil {
			return fmt.Errorf("refusal message: %w", err)
		}
	}
	for i, wd := range want.Details {
		if err := matchOneDetail(wd, ce, scope); err != nil {
			return fmt.Errorf("refusal details[%d]: %w", i, err)
		}
	}
	return nil
}

// ExpectTransportError is a refusal that happened BELOW the surface.
type ExpectTransportError struct {
	// Message asserts what the transport reported. Required by the spec:
	// at this layer there is no code, so the message is the only thing
	// separating a hidden tool from a malformed request or a dead stack.
	Message any
}

// matchTransportError asserts a refusal the surface never answered.
//
// Two failure modes get their own message because they are the two ways
// this assertion goes wrong, and they mean opposite things:
//
//   - the call SUCCEEDED: whatever was supposed to turn it away did not.
//     For the case this exists for — an ACL-hidden MCP tool — that is the
//     caller reaching a tool they may not use.
//   - the SURFACE answered with a code: the refusal is real but it came
//     from a different layer than the step describes. Accepting it would
//     let the case keep passing after the mechanism moved, which is the
//     whole reason `expect_error` requires a code in the first place.
func matchTransportError(want *ExpectTransportError, err error, scope *runtime.Scope) error {
	if err == nil {
		return fmt.Errorf("expected the call to be refused below the surface, but it SUCCEEDED — " +
			"whatever should have turned it away did not")
	}
	var ce *CallError
	if errors.As(err, &ce) {
		return fmt.Errorf("expected a refusal BELOW the surface, but the surface answered %s: %s — "+
			"that is a coded refusal, so assert it with `expect_error` (a step that accepted both "+
			"would keep passing after the refusal changed layers)", ce.Code, ce.Message)
	}
	return runtime.MatchExpect(
		map[string]any{"message": want.Message},
		map[string]any{"message": err.Error()},
		scope)
}

// matchOneDetail looks for ONE expected detail among the refusal's
// details. A miss reports what the surface DID return: the failure is
// almost always "a different rule fired", and naming the rules that
// did fire is the difference between a diagnosis and a retry.
func matchOneDetail(want ExpectErrorDetail, ce *CallError, scope *runtime.Scope) error {
	if len(ce.Details) == 0 {
		return fmt.Errorf("the refusal carried NO per-field details, so no rule can be named — got code %s with message %q", ce.Code, ce.Message)
	}
	for _, got := range ce.Details {
		if want.Field != "" && want.Field != got.Field {
			continue
		}
		if want.Code != "" && want.Code != got.Code {
			continue
		}
		if want.Message != nil {
			if err := runtime.MatchExpect(map[string]any{"message": want.Message}, map[string]any{"message": got.Message}, scope); err != nil {
				continue
			}
		}
		return nil
	}
	return fmt.Errorf("no returned detail matches %s; the refusal carried %s", describeWantDetail(want), describeGotDetails(ce.Details))
}

func describeWantDetail(w ExpectErrorDetail) string {
	parts := make([]string, 0, 3)
	if w.Field != "" {
		parts = append(parts, fmt.Sprintf("field=%q", w.Field))
	}
	if w.Code != "" {
		parts = append(parts, fmt.Sprintf("code=%q", w.Code))
	}
	if w.Message != nil {
		parts = append(parts, fmt.Sprintf("message=%v", w.Message))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func describeGotDetails(got []CallErrorDetail) string {
	parts := make([]string, 0, len(got))
	for _, d := range got {
		parts = append(parts, fmt.Sprintf("{field=%q code=%q message=%q}", d.Field, d.Code, d.Message))
	}
	return strings.Join(parts, ", ")
}
