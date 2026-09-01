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
	events EventSubscriber
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
	expanded, err := runtime.Expand(s.Input, scope)
	if err != nil {
		return fmt.Errorf("expand input: %w", err)
	}
	input, _ := expanded.(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	token := ""
	// REV-162 — a capability-link endpoint IS auth-required, but its
	// credential rides in the URL. Injecting the scenario's bearer
	// token here would be worse than redundant: the case would pass
	// while exercising a request no recipient of such a link can
	// make, and it would fail outright in any scenario that never
	// signed anyone in — which is the only honest way to write this
	// case in the first place.
	if s.Endpoint.AuthRequired && !s.Endpoint.CredentialInURL {
		v, ok := scope.Get("auth.token")
		if !ok {
			return fmt.Errorf("auth-required endpoint but no auth.token captured upstream in this scenario")
		}
		token = fmt.Sprint(v)
	}
	headers, err := expandHeaders(s.Headers, scope)
	if err != nil {
		return fmt.Errorf("expand headers: %w", err)
	}

	// Open every event subscription BEFORE the call so an async event the
	// call triggers can't land before we're listening (the stream has no
	// replay). One subscription per awaited topic keeps each Await from
	// consuming a sibling's frame. Subscriptions are live once Subscribe
	// returns.
	var subs []Subscription
	defer func() {
		for _, sub := range subs {
			_ = sub.Close()
		}
	}()
	if len(s.AwaitEvents) > 0 {
		if cfg.events == nil {
			return fmt.Errorf("step awaits event %q but no event subscriber configured (pass runner.WithEventSubscriber)", s.AwaitEvents[0].Topic)
		}
		evToken := token
		if evToken == "" {
			// The /w17-events stream is gated by the same realm as REST;
			// reuse the scenario's captured token even when this step's own
			// endpoint needs none.
			if v, ok := scope.Get("auth.token"); ok {
				evToken = fmt.Sprint(v)
			}
		}
		for _, ae := range s.AwaitEvents {
			// The step's headers ride the subscribe too. The stream is
			// authenticated by the same auth method as the call, and that
			// method reads client-chosen headers: `W17-Org` picks the active
			// org, whose id is the label the /w17-events hub partitions by.
			// Subscribing without them would connect a label-less principal
			// — which on an isolating surface is entitled to NO event — so
			// the await would time out on a surface that works.
			sub, err := cfg.events.Subscribe(ctx, ae.Path, []string{ae.Topic}, evToken, headers)
			if err != nil {
				return err
			}
			subs = append(subs, sub)
		}
	}

	resp, err := caller.Call(ctx, s.Endpoint, input, token, headers, s.Files)
	if s.ExpectTransportError != nil {
		if err := matchTransportError(s.ExpectTransportError, err, scope); err != nil {
			return err
		}
	} else if s.ExpectError != nil {
		if err := matchCallError(s.ExpectError, err, scope); err != nil {
			return err
		}
	} else {
		if err != nil {
			return err
		}
		if err := runtime.MatchExpect(s.Expect, resp, scope); err != nil {
			return err
		}
	}

	for i, ae := range s.AwaitEvents {
		timeout := time.Duration(ae.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = DefaultAwaitTimeoutMs * time.Millisecond
		}
		ev, err := subs[i].Await(ctx, ae.Topic, timeout)
		if err != nil {
			return fmt.Errorf("await_event %q: %w", ae.Topic, err)
		}
		if err := runtime.MatchExpect(ae.Match, ev.Data, scope); err != nil {
			return fmt.Errorf("await_event %q payload: %w", ae.Topic, err)
		}
	}
	return nil
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
