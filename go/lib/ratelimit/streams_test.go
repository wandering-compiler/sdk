package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func streamReq(remote string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	r.RemoteAddr = remote
	return r
}

// The stream cap is opt-in: unset (or 0) means none, the scoped and
// unscoped variables set it like the limiter's other values, and a value
// that is not a whole number caps nothing (it is logged).
func TestFromEnv_MaxStreamsIsOffUnlessSet(t *testing.T) {
	if got := FromEnv("SVCX").MaxStreams; got != 0 {
		t.Errorf("unset MaxStreams = %d, want 0 (no cap)", got)
	}
	t.Setenv("SVCX_RATELIMIT_MAX_STREAMS", "4")
	if got := FromEnv("SVCX").MaxStreams; got != 4 {
		t.Errorf("MaxStreams = %d, want 4", got)
	}
	t.Setenv("SVCX_PUBLIC_RATELIMIT_MAX_STREAMS", "7")
	if got := FromEnvScope("SVCX", "PUBLIC", "").MaxStreams; got != 7 {
		t.Errorf("scoped MaxStreams = %d, want 7", got)
	}
	t.Setenv("SVCX_PUBLIC_RATELIMIT_MAX_STREAMS", "lots")
	if got := FromEnvScope("SVCX", "PUBLIC", "").MaxStreams; got != 0 {
		t.Errorf("malformed MaxStreams = %d, want 0: a value that cannot be read caps nothing", got)
	}
}

// Without a cap every stream opens; a nil limiter likewise.
func TestOpenStream_NoCapAdmitsEveryStream(t *testing.T) {
	l := New(Config{PerMinute: 30, Burst: 1})
	for i := range 100 {
		if ok, _ := l.OpenStream(streamReq("203.0.113.9:1")); !ok {
			t.Fatalf("stream %d refused without a cap", i)
		}
	}
	if ok, release := (*Limiter)(nil).OpenStream(streamReq("203.0.113.9:1")); !ok {
		t.Fatal("nil limiter refused a stream")
	} else {
		release()
	}
}

// With a cap, one client holds at most MaxStreams open, other clients are
// unaffected, a release frees exactly one slot however often it is called,
// and opening spends no token.
func TestOpenStream_CapsOpenStreamsPerClient(t *testing.T) {
	l := New(Config{PerMinute: 0.001, Burst: 1, MaxStreams: 2})
	a := func() (bool, func()) { return l.OpenStream(streamReq("203.0.113.9:1")) }
	ok1, rel1 := a()
	ok2, _ := a()
	if !ok1 || !ok2 {
		t.Fatal("streams inside the cap refused")
	}
	if ok, _ := a(); ok {
		t.Fatal("third concurrent stream admitted over a cap of 2")
	}
	if ok, _ := l.OpenStream(streamReq("198.51.100.7:1")); !ok {
		t.Fatal("another client was refused by the first client's streams")
	}
	rel1()
	rel1()
	if ok, _ := a(); !ok {
		t.Fatal("a released slot was not freed")
	}
	if ok, _ := a(); ok {
		t.Fatal("a second release freed a second slot")
	}
	if !l.AllowRequest(streamReq("203.0.113.9:1")) {
		t.Fatal("opening streams spent the client's only token")
	}
}

// The stream cap is a bound of its own: switching the per-minute limit off —
// the scope's, the transport's, or a plain config's — MUST NOT switch it off
// too. A limiter whose rate is off still counts open streams, and spends no
// token on anything.
func TestOpenStream_CapHoldsWhileTheRateIsOff(t *testing.T) {
	open2 := func(t *testing.T, l *Limiter) {
		t.Helper()
		r := httptest.NewRequest("GET", "/s", nil)
		r.RemoteAddr = "203.0.113.9:4000"
		ok1, _ := l.OpenStream(r)
		ok2, _ := l.OpenStream(r)
		if !ok1 || ok2 {
			t.Fatalf("MAX_STREAMS=1 with the rate off: first=%v second=%v, want true/false", ok1, ok2)
		}
		other := httptest.NewRequest("GET", "/s", nil)
		other.RemoteAddr = "198.51.100.4:4000"
		if ok, _ := l.OpenStream(other); !ok {
			t.Fatal("another client was refused")
		}
		// The rate is off: nothing is token-limited.
		for i := 0; i < 500; i++ {
			if !l.AllowRequest(r) || !l.Allow(ctxFrom("203.0.113.9:1")) {
				t.Fatalf("a limiter whose rate is off refused request %d", i)
			}
			if ok, done := l.ReserveRequest(r); !ok {
				t.Fatalf("a limiter whose rate is off refused reservation %d", i)
			} else {
				done(true)
			}
			if ok, charge := l.AllowChargeableRequest(r); !ok {
				t.Fatalf("a limiter whose rate is off refused chargeable %d", i)
			} else {
				charge()
			}
		}
	}
	t.Run("config", func(t *testing.T) {
		open2(t, New(Config{PerMinute: 0, MaxStreams: 1}))
	})
	t.Run("transport off", func(t *testing.T) {
		t.Setenv("VX_PUBLIC_RATELIMIT_MAX_STREAMS", "1")
		t.Setenv("VX_PUBLIC_RATELIMIT_REST_PER_MINUTE", "0")
		m := NewMethods("VX", "PUBLIC", "REST", Method{Key: "K", PerMinute: 20, Burst: 4})
		if m.For("K") != nil {
			t.Fatal("the transport is off, yet the method is limited")
		}
		open2(t, m.Streams())
	})
	t.Run("scope off, method revived", func(t *testing.T) {
		t.Setenv("VY_PUBLIC_RATELIMIT_MAX_STREAMS", "1")
		t.Setenv("VY_PUBLIC_RATELIMIT_PER_MINUTE", "0")
		t.Setenv("VY_PUBLIC_RATELIMIT_K_PER_MINUTE", "30")
		m := NewMethods("VY", "PUBLIC", "REST", Method{Key: "K"})
		if m.For("K") == nil {
			t.Fatal("the method's own env no longer revives it")
		}
		open2(t, m.Streams())
	})
	t.Run("no cap and no rate is nil", func(t *testing.T) {
		if l := New(Config{PerMinute: 0}); l != nil {
			t.Fatalf("a config with neither a rate nor a cap built a limiter: %+v", l.cfg)
		}
	})
}
