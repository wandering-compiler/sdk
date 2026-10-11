package eventbus_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// After Drain the memory bus delivers nothing. An emit it cannot deliver
// must be refused and reported as a failure, not counted as a success —
// otherwise the event is lost without a trace while the observer records it
// as emitted. Drain is bus-wide, so this holds for every channel.
func TestMemoryBus_DispatchAfterDrainIsRefused(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})
	sub, err := bus.Subscriber(context.Background(), "a")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int32
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	for _, ch := range []string{"a", "b"} {
		err := bus.Dispatch(context.Background(), ch, "t", wrapperspb.String("x"))
		if !errors.Is(err, eventbus.ErrBusDraining) {
			t.Errorf("Dispatch(%q) after Drain = %v, want ErrBusDraining", ch, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := delivered.Load(); n != 0 {
		t.Errorf("delivered %d events after Drain", n)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.emitSuccess) != 0 {
		t.Errorf("OnEmitSuccess fired %d times for emits nothing received", len(obs.emitSuccess))
	}
	if len(obs.emitFailure) != 2 {
		t.Errorf("OnEmitFailure fired %d times, want 2", len(obs.emitFailure))
	}
}

// A Subscribe after Drain registers a handler nothing will ever call; it is
// refused the way the broker adapters refuse it.
func TestMemoryBus_SubscribeAfterDrainIsRefused(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	first, _ := bus.Subscriber(context.Background(), "a")
	if err := first.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	for _, ch := range []string{"a", "b"} {
		sub, err := bus.Subscriber(context.Background(), ch)
		if err != nil {
			t.Fatalf("Subscriber: %v", err)
		}
		err = sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error { return nil })
		if !errors.Is(err, eventbus.ErrSubscriberDraining) {
			t.Errorf("Subscribe(%q) after Drain = %v, want ErrSubscriberDraining", ch, err)
		}
	}
}
