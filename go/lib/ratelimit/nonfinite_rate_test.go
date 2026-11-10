package ratelimit

import "testing"

// A rate that is not a finite number is malformed input: it MUST leave the
// previous value standing, like any other value ParseFloat refuses. Read as a
// number, NaN fails every `<= 0` comparison, so the limiter reported itself
// on while its bucket never ran dry.
func TestPerMinute_NotAFiniteNumberIsMalformed(t *testing.T) {
	for _, v := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity"} {
		t.Run("scope "+v, func(t *testing.T) {
			t.Setenv("NF_PUBLIC_RATELIMIT_PER_MINUTE", v)
			cfg := FromEnvScope("NF", "PUBLIC", "")
			if cfg.PerMinute != DefaultPerMinute {
				t.Fatalf("PER_MINUTE=%s: PerMinute = %v, want the default %v standing", v, cfg.PerMinute, float64(DefaultPerMinute))
			}
			assertBounded(t, New(cfg), cfg.Burst)
		})
		t.Run("method "+v, func(t *testing.T) {
			t.Setenv("NM_PUBLIC_RATELIMIT_K_PER_MINUTE", v)
			m := NewMethods("NM", "PUBLIC", "REST", Method{Key: "K", PerMinute: 12})
			l := m.For("K")
			if l == nil {
				t.Fatalf("<KEY>_PER_MINUTE=%s switched the method off", v)
			}
			if l.cfg.PerMinute != 12 {
				t.Fatalf("<KEY>_PER_MINUTE=%s: PerMinute = %v, want the declared 12 standing", v, l.cfg.PerMinute)
			}
			assertBounded(t, l, l.cfg.Burst)
		})
	}
}

func assertBounded(t *testing.T, l *Limiter, burst int) {
	t.Helper()
	n := 0
	for n < 1000 && l.Allow(ctxFrom("203.0.113.9:1")) {
		n++
	}
	if n != burst {
		t.Fatalf("admitted %d calls from one address, want the burst %d", n, burst)
	}
}
