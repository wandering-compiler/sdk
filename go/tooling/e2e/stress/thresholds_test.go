package stress

import (
	"strings"
	"testing"
	"time"
)

// A run that measured nothing must not pass its thresholds: every figure
// of an empty measurement is zero, which satisfies every latency gate.
func TestCheck_ZeroMeasuredRequestsFails(t *testing.T) {
	r := newReport("x", ModePool, 1, nil, 0, 0, 0, 0, time.Second)
	for name, th := range map[string]*Thresholds{
		"latency":    {P95: time.Second},
		"error rate": {ErrorRate: 0.01},
		"empty":      {},
	} {
		err := r.Check(th)
		if err == nil || !strings.Contains(err.Error(), "no requests were measured") {
			t.Errorf("%s: zero measured requests passed: %v", name, err)
		}
	}
	if err := r.Check(nil); err != nil {
		t.Errorf("report-only (no thresholds) stays a pass: %v", err)
	}
}

// `error_rate: 0` set on purpose means no errors allowed; an unset zero is
// no gate, as before.
func TestCheck_ErrorRateZeroWhenSetAllowsNoErrors(t *testing.T) {
	lat := []int64{1e6, 1e6, 1e6, 1e6}
	oneError := newReport("x", ModePool, 1, lat, 3, 0, 1, 0, time.Second)
	if err := oneError.Check(&Thresholds{ErrorRate: 0, ErrorRateSet: true}); err == nil || !strings.Contains(err.Error(), "error_rate") {
		t.Errorf("error_rate 0 (set) let an error through: %v", err)
	}
	if err := oneError.Check(&Thresholds{}); err != nil {
		t.Errorf("an unset error rate is no gate: %v", err)
	}
	clean := newReport("x", ModePool, 1, lat, 4, 0, 0, 0, time.Second)
	if err := clean.Check(&Thresholds{ErrorRate: 0, ErrorRateSet: true}); err != nil {
		t.Errorf("an error-free run passes error_rate 0: %v", err)
	}
}
