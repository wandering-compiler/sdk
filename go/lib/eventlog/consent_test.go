package eventlog

import (
	"testing"
	"time"
)

func TestCapAuthCacheTTL_KeepsWhatFitsAndCapsTheRest(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, 0},
		{30 * time.Second, 30 * time.Second},
		{ConsentCacheCeiling, ConsentCacheCeiling},
		{time.Hour, ConsentCacheCeiling},
	} {
		if got := CapAuthCacheTTL(c.in); got != c.want {
			t.Errorf("CapAuthCacheTTL(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}
