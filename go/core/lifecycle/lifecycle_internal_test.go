package lifecycle

import (
	"testing"
	"time"
)

type fakeGRPC struct {
	graceful bool
	hard     bool
}

func (f *fakeGRPC) GracefulStop() { f.graceful = true }
func (f *fakeGRPC) Stop()         { f.hard = true }

// Zero is the documented opt-out: block until every RPC finishes, no hard
// stop. Exercised through the seam because a real unbounded GracefulStop
// has no observable "and it did NOT force" moment.
func TestStopGRPCZeroTimeoutIsUnbounded(t *testing.T) {
	fake := &fakeGRPC{}
	stopGRPC(fake, 0)
	if !fake.graceful || fake.hard {
		t.Fatalf("zero timeout must be an unbounded graceful stop; got graceful=%v hard=%v", fake.graceful, fake.hard)
	}
}

// A negative value is treated as the same opt-out, not as an instant kill.
func TestStopGRPCNegativeTimeoutIsUnbounded(t *testing.T) {
	fake := &fakeGRPC{}
	stopGRPC(fake, -time.Second)
	if !fake.graceful || fake.hard {
		t.Fatalf("negative timeout must not force a hard stop; got graceful=%v hard=%v", fake.graceful, fake.hard)
	}
}
