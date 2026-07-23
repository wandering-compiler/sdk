package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

func newScope() *runtime.Scope { return runtime.NewRun().NewScope() }

// stepFakeCaller is a controllable Caller for exercising runStep / RunSteps
// branches without a live transport.
type stepFakeCaller struct {
	resp  map[string]any
	err   error
	calls int
}

func (f *stepFakeCaller) Call(_ context.Context, _ Endpoint, _ map[string]any, _ string, _ map[string]string) (map[string]any, error) {
	f.calls++
	return f.resp, f.err
}

func restStep(label string) Step {
	return Step{
		Endpoint: Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/m"},
		Input:    map[string]any{"a": 1},
		Label:    label,
	}
}

func TestRunSteps_RepeatFailureWrapsIter(t *testing.T) {
	fc := &stepFakeCaller{err: errors.New("boom")}
	s := restStep("repeated")
	s.Repeat = 2
	err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": fc})
	if err == nil || !contains(err.Error(), "iter 1/2") {
		t.Errorf("repeat failure should mention iteration: %v", err)
	}
}

func TestRunStep_NoCallerForTransport(t *testing.T) {
	s := restStep("x")
	s.Endpoint.Transport = "grpc-web" // no caller registered
	if err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{}); err == nil {
		t.Error("missing caller should error")
	}
}

func TestRunStep_ExpandInputError(t *testing.T) {
	s := restStep("x")
	s.Input = map[string]any{"a": "${nope}"}
	fc := &stepFakeCaller{resp: map[string]any{}}
	if err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": fc}); err == nil {
		t.Error("unresolved input token should error")
	}
}

func TestRunStep_NilInputDefaultsToEmpty(t *testing.T) {
	s := restStep("x")
	s.Input = nil // expands to nil → defaulted to empty object
	fc := &stepFakeCaller{resp: map[string]any{}}
	if err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": fc}); err != nil {
		t.Errorf("nil input should default cleanly: %v", err)
	}
	if fc.calls != 1 {
		t.Errorf("caller hit %d times, want 1", fc.calls)
	}
}

func TestRunStep_ExpandHeadersError(t *testing.T) {
	s := restStep("x")
	s.Headers = map[string]string{"X-Trace": "${nope}"}
	fc := &stepFakeCaller{resp: map[string]any{}}
	if err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": fc}); err == nil {
		t.Error("unresolved header token should error")
	}
}

func TestRunStep_CallerError(t *testing.T) {
	s := restStep("x")
	fc := &stepFakeCaller{err: errors.New("transport down")}
	if err := RunSteps(context.Background(), newScope(), []Step{s}, map[string]Caller{"rest": fc}); err == nil {
		t.Error("caller error should propagate")
	}
}

func TestEmitStep_JSONFormat(t *testing.T) {
	SetFormat("json")
	defer SetFormat("text")
	// a pass and a fail, both emitted through the JSON branch
	fcOK := &stepFakeCaller{resp: map[string]any{}}
	if err := RunSteps(context.Background(), newScope(), []Step{restStep("ok")}, map[string]Caller{"rest": fcOK}); err != nil {
		t.Fatalf("json pass: %v", err)
	}
	fcErr := &stepFakeCaller{err: errors.New("nope")}
	if err := RunSteps(context.Background(), newScope(), []Step{restStep("bad")}, map[string]Caller{"rest": fcErr}); err == nil {
		t.Error("json fail should error")
	}
}

func TestRESTCaller_Call_BuildRequestError(t *testing.T) {
	// an invalid HTTP method (contains a space) passes ResolveREST but
	// fails http.NewRequestWithContext's method-token validation.
	c := NewRESTCaller("http://127.0.0.1:1", nil)
	ep := Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "BAD METHOD", PathTemplate: "/m"}
	if _, err := c.Call(context.Background(), ep, map[string]any{"a": 1}, "", nil); err == nil {
		t.Error("invalid method should fail request build")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
