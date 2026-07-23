package redis

import (
	"testing"
	"time"
)

// TestTTLDuration pins the stored-millis → time.Duration conversion used by
// RESTORE: non-positive means "no expiry" (0), positive scales by ms.
func TestTTLDuration(t *testing.T) {
	cases := []struct {
		millis int64
		want   time.Duration
	}{
		{0, 0},
		{-5, 0},
		{1, time.Millisecond},
		{1500, 1500 * time.Millisecond},
	}
	for _, c := range cases {
		if got := ttlDuration(c.millis); got != c.want {
			t.Errorf("ttlDuration(%d) = %v, want %v", c.millis, got, c.want)
		}
	}
}
