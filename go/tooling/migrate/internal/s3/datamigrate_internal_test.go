package s3

import "testing"

// TestDataCursor_AddStartedDedup pins addStarted's idempotency guard:
// re-marking an op already in StartedOps is a no-op (no duplicate
// entry), so a retried start-marker write can't grow the cursor without
// bound. The dedup early-return is unreachable through the normal apply
// flow (guardTransformResume refuses a re-started op before re-marking),
// so it is exercised directly.
func TestDataCursor_AddStartedDedup(t *testing.T) {
	c := &dataCursor{}
	c.addStarted(2)
	c.addStarted(2) // duplicate — must be dropped
	c.addStarted(5)
	if len(c.StartedOps) != 2 {
		t.Fatalf("StartedOps = %v, want exactly [2 5]", c.StartedOps)
	}
	if !c.hasStarted(2) || !c.hasStarted(5) {
		t.Errorf("hasStarted lost an index: %v", c.StartedOps)
	}
	if c.hasStarted(9) {
		t.Error("hasStarted(9) = true, want false")
	}
}
