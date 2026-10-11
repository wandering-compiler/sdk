package eventbus_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// panickingObserver panics in every callback, including the latency
// hook, and counts how often it was called.
type panickingObserver struct{ calls atomic.Int64 }

func (p *panickingObserver) boom() { p.calls.Add(1); panic("observer backend exploded") }

func (p *panickingObserver) OnEmitSuccess(string, string)           { p.boom() }
func (p *panickingObserver) OnEmitFailure(string, string, error)    { p.boom() }
func (p *panickingObserver) OnDeliverSuccess(string, string)        { p.boom() }
func (p *panickingObserver) OnDeliverFailure(string, string, error) { p.boom() }
func (p *panickingObserver) OnDrop(string, string, string)          { p.boom() }
func (p *panickingObserver) OnDeliverComplete(string, string, time.Duration) {
	p.boom()
}

// dispatchMustNotPanic calls Dispatch and turns an escaped panic into a
// test failure instead of a crashed test binary.
func dispatchMustNotPanic(t *testing.T, bus eventbus.Dispatcher, channel, topic string) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Dispatch(%q, %q) panicked through a panicking Observer: %v", channel, topic, r)
		}
	}()
	return bus.Dispatch(context.Background(), channel, topic, wrapperspb.String("x"))
}

// INVARIANT: a panicking Observer never reaches the emitting caller —
// neither on success, nor on a refused emit, nor on the delivery side —
// and the event is still delivered.
func TestMemoryBus_PanickingObserverDoesNotBreakEmit(t *testing.T) {
	obs := &panickingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})
	sub, _ := bus.Subscriber(context.Background(), "default")

	var delivered atomic.Int64
	_ = sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	})

	for i := 0; i < 2; i++ {
		if err := dispatchMustNotPanic(t, bus, "default", "user.created"); err != nil {
			t.Fatalf("Dispatch #%d = %v, want nil (the event was published)", i, err)
		}
	}
	// Refused emit: the failure callback panics, the caller still gets
	// the refusal as an error.
	if err := dispatchMustNotPanic(t, bus, "bad channel!", "user.created"); err == nil {
		t.Fatal("Dispatch on an invalid channel = nil, want the validation error")
	}
	_ = sub.Drain(timeoutCtx(t, 2*time.Second))

	if got := delivered.Load(); got != 2 {
		t.Errorf("delivered = %d, want 2", got)
	}
	// emit success x2, emit failure x1, deliver success + latency x2.
	if got := obs.calls.Load(); got < 7 {
		t.Errorf("observer calls = %d, want >= 7 (every callback still invoked)", got)
	}
}

func TestRedisBus_PanickingObserverDoesNotBreakEmit_Embedded(t *testing.T) {
	obs := &panickingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "panicking-observer",
		Observer:    obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	assertBrokerSurvivesPanickingObserver(t, bus, obs)
}

func TestNatsBus_PanickingObserverDoesNotBreakEmit_Embedded(t *testing.T) {
	obs := &panickingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "panicking-observer",
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	assertBrokerSurvivesPanickingObserver(t, bus, obs)
}

type brokerBus interface {
	eventbus.Dispatcher
	Subscriber(ctx context.Context, channel string) (eventbus.Subscriber, error)
}

func assertBrokerSurvivesPanickingObserver(t *testing.T, bus brokerBus, obs *panickingObserver) {
	t.Helper()
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int64
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := dispatchMustNotPanic(t, bus, "default", "user.created"); err != nil {
			t.Fatalf("Dispatch #%d = %v, want nil (the event was published)", i, err)
		}
	}
	if err := dispatchMustNotPanic(t, bus, "bad channel!", "user.created"); err == nil {
		t.Fatal("Dispatch on an invalid channel = nil, want the validation error")
	}
	// The consume loop survives the delivery-side callbacks panicking and
	// keeps delivering.
	waitFor(t, 10*time.Second, "both events delivered", func() bool { return delivered.Load() >= 2 })
	if obs.calls.Load() == 0 {
		t.Error("observer never called")
	}
}
