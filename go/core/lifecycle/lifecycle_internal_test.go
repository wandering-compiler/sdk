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

// blockingGRPC never finishes either stop, like a server whose handler
// ignores its cancelled context.
type blockingGRPC struct{ hard chan struct{} }

func (b *blockingGRPC) GracefulStop() { select {} }
func (b *blockingGRPC) Stop() {
	close(b.hard)
	select {}
}

// The bound covers the forced stop too: when Stop itself does not return,
// stopGRPC still returns after the graceful bound plus one more bound.
func TestStopGRPCBoundsAHardStopThatDoesNotReturn(t *testing.T) {
	b := &blockingGRPC{hard: make(chan struct{})}
	start := time.Now()
	returned := make(chan struct{})
	go func() {
		stopGRPC(b, 50*time.Millisecond)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stopGRPC did not return while Stop was blocked")
	}
	select {
	case <-b.hard:
	default:
		t.Fatal("hard stop was never attempted")
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("returned after %s, before the hard stop had its own bound", took)
	}
}
