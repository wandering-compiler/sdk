package ratelimit

import (
	"testing"
	"time"
)

func TestNewPrincipal_OffUnlessDeclaredOrSwitchedOn(t *testing.T) {
	if l := NewPrincipal("PT1", "REST", 0, 0); l != nil {
		t.Fatal("nothing declared and nothing set built a limiter")
	}
	t.Setenv("PT2_PRINCIPAL_RATELIMIT_PER_MINUTE", "120")
	l := NewPrincipal("PT2", "REST", 0, 0)
	if l == nil || l.cfg.PerMinute != 120 || l.cfg.Burst != DefaultPrincipalBurst {
		t.Fatalf("switched on by the deployment: %+v", l)
	}
}

func TestNewPrincipal_Precedence(t *testing.T) {
	if l := NewPrincipal("PT3", "RPC", 60, 5); l == nil || l.cfg.PerMinute != 60 || l.cfg.Burst != 5 {
		t.Fatalf("declared: %+v", l)
	}
	t.Setenv("PT4_PRINCIPAL_RATELIMIT_PER_MINUTE", "100")
	t.Setenv("PT4_PRINCIPAL_RATELIMIT_RPC_PER_MINUTE", "200")
	t.Setenv("PT4_PRINCIPAL_RATELIMIT_RPC_BURST", "7")
	if l := NewPrincipal("PT4", "RPC", 60, 5); l.cfg.PerMinute != 200 || l.cfg.Burst != 7 {
		t.Fatalf("transport over scope over declared: %+v", l.cfg)
	}
	if l := NewPrincipal("PT4", "REST", 60, 5); l.cfg.PerMinute != 100 || l.cfg.Burst != 5 {
		t.Fatalf("the other transport takes the scope: %+v", l.cfg)
	}
	t.Setenv("PT5_PRINCIPAL_RATELIMIT_REST_PER_MINUTE", "0")
	if l := NewPrincipal("PT5", "REST", 60, 5); l != nil {
		t.Fatal("<= 0 did not switch a declared limit off")
	}
}

// One bucket per principal; a call spends its cost; another principal is not
// touched; refilled at the rate.
func TestAllowPrincipal_BucketPerPrincipalAndCost(t *testing.T) {
	l := NewPrincipal("PT6", "REST", 60, 10) // one token a second
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	if ok, _ := l.AllowPrincipal("alice", 7); !ok {
		t.Fatal("first call refused")
	}
	ok, wait := l.AllowPrincipal("alice", 4)
	if ok || wait != time.Second {
		t.Fatalf("over budget: ok=%v wait=%v, want refused, 1s", ok, wait)
	}
	if ok, _ := l.AllowPrincipal("bob", 10); !ok {
		t.Fatal("another principal spent alice's budget")
	}
	now = now.Add(time.Second)
	if ok, _ := l.AllowPrincipal("alice", 4); !ok {
		t.Fatal("not refilled at the rate")
	}
	for _, c := range []struct {
		p    string
		cost int
	}{{"", 5}, {"alice", 0}} {
		if ok, _ := l.AllowPrincipal(c.p, c.cost); !ok {
			t.Errorf("%+v refused; an exempt cost or an id-less principal is admitted", c)
		}
	}
	var nilL *Limiter
	if ok, _ := nilL.AllowPrincipal("alice", 1); !ok {
		t.Error("nil limiter refused")
	}
}

func TestAllowPrincipal_CostAboveBurstIsChargedTheBurst(t *testing.T) {
	l := NewPrincipal("PT7", "REST", 60, 3)
	if ok, _ := l.AllowPrincipal("alice", 50); !ok {
		t.Fatal("a cost above the burst can never run")
	}
	if ok, _ := l.AllowPrincipal("alice", 1); ok {
		t.Fatal("it was not charged the whole burst")
	}
}
