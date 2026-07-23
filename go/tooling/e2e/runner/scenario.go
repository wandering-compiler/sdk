package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	// AwaitEvents, when non-empty, asserts that each listed public event
	// lands on the gateway's `/w17-events` SSE stream after this step's
	// call. The runner subscribes to every one BEFORE the call (the stream
	// has no replay), issues the call, then waits + matches each payload. A
	// step routinely awaits a single event; a method that emits several
	// public events lists them all.
	AwaitEvents []AwaitEvent
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
	if s.Endpoint.AuthRequired {
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
			sub, err := cfg.events.Subscribe(ctx, ae.Path, []string{ae.Topic}, evToken)
			if err != nil {
				return err
			}
			subs = append(subs, sub)
		}
	}

	resp, err := caller.Call(ctx, s.Endpoint, input, token, headers)
	if err != nil {
		return err
	}
	if err := runtime.MatchExpect(s.Expect, resp, scope); err != nil {
		return err
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
