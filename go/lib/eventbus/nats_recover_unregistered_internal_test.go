package eventbus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	natsserver "github.com/nats-io/nats-server/v2/test"
)

type fakeConsumeContext struct {
	stopped atomic.Bool
	closed  chan struct{}
}

func newFakeConsumeContext() *fakeConsumeContext {
	return &fakeConsumeContext{closed: make(chan struct{})}
}

func (f *fakeConsumeContext) Stop() {
	if f.stopped.CompareAndSwap(false, true) {
		close(f.closed)
	}
}
func (f *fakeConsumeContext) Drain()                  { f.Stop() }
func (f *fakeConsumeContext) Closed() <-chan struct{} { return f.closed }

// A consumer recovery that finishes before Subscribe has registered its
// handle must not give up: the original pull loop is the one the broker
// lost, so stopping the new loop leaves the subscription with no consumer
// for good. The recovery waits for the registration and swaps its loop in.
func TestNatsRecoverConsumer_WaitsForTheHandleToBeRegistered(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	bus, err := NewNatsBus(NatsBusOptions{DSN: srv.ClientURL(), DurablePrefix: "recover-unregistered"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	fac, err := bus.Subscriber(context.Background(), "recoverreg")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	s := fac.(*natsSubscriber)

	handle := newNatsConsumeHandle()
	recovered := newFakeConsumeContext()
	start := func(context.Context) (jetstream.ConsumeContext, error) { return recovered, nil }

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.recoverConsumer(context.Background(), handle, "recover-unregistered-recoverreg", true, start)
	}()

	// The subscribing goroutine registers only now, after the recovery has
	// re-created the consumer.
	time.Sleep(300 * time.Millisecond)
	original := newFakeConsumeContext()
	s.mu.Lock()
	handle.cctx = original
	s.handles = append(s.handles, handle)
	s.mu.Unlock()
	handle.markRegistered()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recoverConsumer did not return")
	}
	s.mu.Lock()
	got := handle.cctx
	s.mu.Unlock()
	if got != jetstream.ConsumeContext(recovered) {
		t.Fatalf("the recovered pull loop was not swapped in (stopped=%v); the subscription is left on the lost consumer", recovered.stopped.Load())
	}
	if !original.stopped.Load() {
		t.Error("the lost consumer's pull loop was not stopped")
	}
}
