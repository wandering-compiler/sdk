package ratelimit

import (
	"net/http/httptest"
	"testing"
)

// spend takes tokens from l for one address until it refuses, and returns how
// many it took (capped, so an unlimited limiter ends the loop).
func spend(l *Limiter, addr string) int {
	n := 0
	for n < 1000 && l.Allow(ctxFrom(addr)) {
		n++
	}
	return n
}

// Each method has its own bucket per address: emptying one leaves the other
// whole. This is the point of the change — a page everyone loads never spends
// a sign-in's budget.
func TestMethods_EachMethodHasItsOwnBucket(t *testing.T) {
	m := NewMethods("MX", "PUBLIC", "REST", Method{Key: "A_SIGN_IN", PerMinute: 30, Burst: 3}, Method{Key: "A_SIGN_UP", PerMinute: 30, Burst: 5})
	if got := spend(m.For("A_SIGN_IN"), "203.0.113.7:1"); got != 3 {
		t.Fatalf("sign-in admitted %d, want its burst 3", got)
	}
	if got := spend(m.For("A_SIGN_UP"), "203.0.113.7:1"); got != 5 {
		t.Fatalf("sign-up admitted %d after the sign-in was spent, want its own burst 5", got)
	}
}

// A method not listed is not limited: For answers nil, which admits all.
func TestMethods_AnUnlistedMethodIsNotLimited(t *testing.T) {
	m := NewMethods("MX", "PUBLIC", "REST", Method{Key: "A_SIGN_IN"})
	if l := m.For("A_GET_LANDING"); l != nil {
		t.Fatalf("an unlisted method got a limiter: %+v", l.cfg)
	}
	if got := spend(m.For("A_GET_LANDING"), "203.0.113.7:1"); got != 1000 {
		t.Fatalf("unlisted method refused after %d", got)
	}
	var nilSet *Methods
	if nilSet.For("A_SIGN_IN") != nil || nilSet.Streams() != nil || nilSet.TrustedProxies() != nil {
		t.Fatal("a nil set must limit nothing")
	}
}

// The order NewMethods documents: the method's env, then the scope switched
// off, then the declared threshold, then the scope's values.
func TestMethods_ConfigPrecedence(t *testing.T) {
	for _, c := range []struct {
		name      string
		env       map[string]string
		meth      Method
		wantOff   bool
		wantRate  float64
		wantBurst int
	}{
		{"declared", nil, Method{Key: "K", PerMinute: 20, Burst: 20}, false, 20, 20},
		{"undeclared takes the library default", nil, Method{Key: "K"}, false, DefaultPerMinute, DefaultBurst},
		{"undeclared takes the scope's env", map[string]string{"MX_PUBLIC_RATELIMIT_PER_MINUTE": "50", "MX_PUBLIC_RATELIMIT_BURST": "7"}, Method{Key: "K"}, false, 50, 7},
		{"declared beats the scope's env", map[string]string{"MX_PUBLIC_RATELIMIT_PER_MINUTE": "50"}, Method{Key: "K", PerMinute: 20, Burst: 4}, false, 20, 4},
		{"the transport's env is the scope's", map[string]string{"MX_PUBLIC_RATELIMIT_REST_PER_MINUTE": "45"}, Method{Key: "K"}, false, 45, DefaultBurst},
		{"the method's env beats its declaration", map[string]string{"MX_PUBLIC_RATELIMIT_K_PER_MINUTE": "90", "MX_PUBLIC_RATELIMIT_K_BURST": "40"}, Method{Key: "K", PerMinute: 20, Burst: 4}, false, 90, 40},
		{"the scope off switches a declared method off", map[string]string{"MX_PUBLIC_RATELIMIT_PER_MINUTE": "0"}, Method{Key: "K", PerMinute: 20, Burst: 4}, true, 0, 0},
		{"the transport off switches it off", map[string]string{"MX_PUBLIC_RATELIMIT_REST_PER_MINUTE": "0"}, Method{Key: "K", PerMinute: 20}, true, 0, 0},
		{"the method's env revives it under a scope off", map[string]string{"MX_PUBLIC_RATELIMIT_PER_MINUTE": "0", "MX_PUBLIC_RATELIMIT_K_PER_MINUTE": "12"}, Method{Key: "K", PerMinute: 20, Burst: 4}, false, 12, 4},
		{"the method's env switches it off alone", map[string]string{"MX_PUBLIC_RATELIMIT_K_PER_MINUTE": "0"}, Method{Key: "K", PerMinute: 20}, true, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			l := NewMethods("MX", "PUBLIC", "REST", c.meth).For("K")
			if c.wantOff {
				if l != nil {
					t.Fatalf("on at %v/%d, want off", l.cfg.PerMinute, l.cfg.Burst)
				}
				return
			}
			if l == nil {
				t.Fatal("off, want on")
			}
			if l.cfg.PerMinute != c.wantRate || l.cfg.Burst != c.wantBurst {
				t.Errorf("got %v/%d, want %v/%d", l.cfg.PerMinute, l.cfg.Burst, c.wantRate, c.wantBurst)
			}
		})
	}
}

// Trusted proxies and the stream cap are the scope's, whatever a method
// declares.
func TestMethods_ScopeSettingsReachEveryMethod(t *testing.T) {
	t.Setenv("MX_PUBLIC_RATELIMIT_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("MX_PUBLIC_RATELIMIT_MAX_STREAMS", "1")
	m := NewMethods("MX", "PUBLIC", "REST", Method{Key: "K", PerMinute: 20, Burst: 4})
	if got := m.For("K").cfg.TrustedProxies; len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Errorf("method's trusted proxies %v", got)
	}
	if got := m.TrustedProxies(); len(got) != 1 {
		t.Errorf("set's trusted proxies %v", got)
	}
	r := httptest.NewRequest("GET", "/s", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	ok, release := m.Streams().OpenStream(r)
	if !ok {
		t.Fatal("the first stream was refused")
	}
	if ok2, _ := m.Streams().OpenStream(r); ok2 {
		t.Error("a second stream passed a cap of 1")
	}
	release()
	if ok3, _ := m.Streams().OpenStream(r); !ok3 {
		t.Error("a stream after the release was refused")
	}
}
