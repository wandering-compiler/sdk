package migratecli

import (
	"context"
	"strings"
	"testing"
)

// An unknown VERB must be refused, not fall through to the server.
//
// Falling through did not produce a confusing message — it produced a stack
// that never came up and never said why. A consumer's compose step ran
// `<binary> schema apply …` after that verb was removed; the binary bound its
// port and sat there, and the step gating on `service_completed_successfully`
// waited forever (deinvo, 2026-09-21).
func TestDispatch_UnknownVerbIsRefused(t *testing.T) {
	for _, verb := range []string{"schema", "zcelanesmysl", "render"} {
		handled, err := Dispatch(context.Background(), []string{verb, "apply"}, Options{})
		if !handled {
			t.Errorf("%q fell through to the server — a compose step gating on this hangs forever", verb)
			continue
		}
		if err == nil {
			t.Errorf("%q was accepted", verb)
			continue
		}
		// The message has to carry the way out, not just the refusal.
		if !strings.Contains(err.Error(), "migrate") || !strings.Contains(err.Error(), "fixtures") {
			t.Errorf("the refusal for %q does not list the commands that exist: %v", verb, err)
		}
		// `schema` is the one people arrive with, so it is answered by name.
		if verb == "schema" && !strings.Contains(err.Error(), "no `schema` command") {
			t.Errorf("the refusal does not say why `schema` is gone: %v", err)
		}
	}
}

// A FLAG still falls through: those belong to the generated main, and this
// package must not interpret them. Without this control the fix would refuse
// every flag the bundle defines.
func TestDispatch_FlagsStillFallThrough(t *testing.T) {
	for _, flag := range []string{"-addr=:9000", "--verbose"} {
		handled, err := Dispatch(context.Background(), []string{flag}, Options{})
		if handled || err != nil {
			t.Errorf("%q was intercepted (handled=%v err=%v) — the generated main owns its flags", flag, handled, err)
		}
	}
}

// And NO argument is still "start the server" — that is what the absence of a
// command means, and the container image depends on it.
func TestDispatch_NoArgsStillStartsTheServer(t *testing.T) {
	handled, err := Dispatch(context.Background(), nil, Options{})
	if handled || err != nil {
		t.Errorf("an argument-less run was intercepted (handled=%v err=%v)", handled, err)
	}
}
