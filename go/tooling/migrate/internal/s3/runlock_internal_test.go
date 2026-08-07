package s3

import (
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/runlock"
)

// TestNow_DefaultIsWallClock pins the production default of now():
// with no injected clock (nowFn nil) it returns the real wall clock,
// bracketed by two time.Now() reads. The test hooks always set nowFn,
// so this is the only exercise of the live-clock fallback.
func TestNow_DefaultIsWallClock(t *testing.T) {
	a := &Applier{} // nowFn == nil
	before := time.Now()
	got := a.now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("now() = %v, want within [%v, %v]", got, before, after)
	}
}

// TestStaleAfter_Default pins both staleAfter() arms: a zero
// lockStale falls back to the runlock package constant, a positive
// override is honoured verbatim.
func TestStaleAfter_Default(t *testing.T) {
	if got := (&Applier{}).staleAfter(); got != runlock.StaleAfter {
		t.Errorf("default staleAfter() = %v, want %v", got, runlock.StaleAfter)
	}
	if got := (&Applier{lockStale: 5 * time.Minute}).staleAfter(); got != 5*time.Minute {
		t.Errorf("override staleAfter() = %v, want 5m", got)
	}
}

// status412 is a non-smithy error exposing only HTTPStatusCode, so it
// drives isPreconditionFailed's second arm (the raw 412 fallback)
// rather than the smithy APIError code match.
type status412 struct{ code int }

func (e status412) Error() string       { return "status error" }
func (e status412) HTTPStatusCode() int { return e.code }

// TestIsPreconditionFailed_StatusCodeFallback pins the HTTPStatusCode
// fallback arm: an error that is not a smithy APIError but reports a
// 412 status is treated as a precondition failure; any other status
// (or a plain error) is not.
func TestIsPreconditionFailed_StatusCodeFallback(t *testing.T) {
	if !isPreconditionFailed(status412{412}) {
		t.Error("status 412 fallback should report precondition failed")
	}
	if isPreconditionFailed(status412{500}) {
		t.Error("status 500 must not report precondition failed")
	}
	if isPreconditionFailed(errors.New("plain")) {
		t.Error("plain error must not report precondition failed")
	}
}
