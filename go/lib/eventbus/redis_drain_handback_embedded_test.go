package eventbus_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A draining Redis subscriber finishes the entry it is running and hands the
// rest of its read batch back, as the NATS adapter Naks a delivery that
// arrives mid-drain: the entries it has not started are claimable by a
// sibling at once, not run by the process that is shutting down (each one
// pushing its exit further out) nor left to age a whole AckWait.
func TestRedisBus_DrainHandsBackUnstartedBatch_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	const ackWait = 5 * time.Second
	newBus := func(name string) *eventbus.RedisBus {
		bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
			DSN:            dsn,
			GroupPrefix:    "handback-subscribers",
			ConsumerName:   name,
			DefaultAckWait: ackWait,
		})
		if err != nil {
			t.Fatalf("NewRedisBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
		return bus
	}

	const n = 5
	a := newBus("process-a")
	for i := 0; i < n; i++ {
		if err := a.Dispatch(context.Background(), "default", "t", wrapperspb.Int32(int32(i))); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
	}

	var ranA, ranB atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	subA, _ := a.Subscriber(context.Background(), "default")
	if err := subA.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		if ranA.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("process A never started its first entry")
	}

	drained := make(chan error, 1)
	go func() { drained <- subA.Drain(timeout(t, 10*time.Second)) }()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}

	b := newBus("process-b")
	subB, _ := b.Subscriber(context.Background(), "default")
	if err := subB.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		ranB.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	waitFor(t, 3*time.Second, "process B takes over the handed-back entries well inside AckWait", func() bool {
		return ranA.Load()+ranB.Load() >= n
	})
	if got := ranA.Load(); got != 1 {
		t.Errorf("draining process ran %d entries, want 1 (the one it had started)", got)
	}
}
