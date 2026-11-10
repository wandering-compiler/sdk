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

type closeDrainBus interface {
	eventbus.Dispatcher
	eventbus.SubscriberFactory
	Close(context.Context) error
}

func closeDrainBuses(t *testing.T, prefix string) map[string]func(t *testing.T) closeDrainBus {
	return map[string]func(t *testing.T) closeDrainBus{
		"nats": func(t *testing.T) closeDrainBus {
			b, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
				DSN: startEmbeddedNATS(t), DurablePrefix: prefix, Observer: eventbus.NopObserver{},
			})
			if err != nil {
				t.Fatalf("NewNatsBus: %v", err)
			}
			return b
		},
		"redis": func(t *testing.T) closeDrainBus {
			b, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
				DSN: startEmbeddedRedis(t), GroupPrefix: prefix, Observer: eventbus.NopObserver{},
			})
			if err != nil {
				t.Fatalf("NewRedisBus: %v", err)
			}
			return b
		},
	}
}

// subscribeBlocking subscribes a handler that signals when it starts and
// returns only once release is closed (or after hold, when release is nil).
func subscribeBlocking(t *testing.T, bus closeDrainBus, hold time.Duration, release <-chan struct{}) (started <-chan struct{}, finished *atomic.Bool) {
	t.Helper()
	sub, err := bus.Subscriber(context.Background(), "closedrain")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	st := make(chan struct{}, 1)
	fin := &atomic.Bool{}
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		select {
		case st <- struct{}{}:
		default:
		}
		if release != nil {
			<-release
		} else {
			time.Sleep(hold)
		}
		fin.Store(true)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "closedrain", "job.run", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case <-st:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}
	return st, fin
}

// Close drains every subscriber before it closes the broker connection: a
// handler already running when Close is called finishes first.
func TestBusClose_WaitsForAnInFlightHandler(t *testing.T) {
	for name, mk := range closeDrainBuses(t, "closedrain-wait") {
		t.Run(name, func(t *testing.T) {
			bus := mk(t)
			_, finished := subscribeBlocking(t, bus, 500*time.Millisecond, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := bus.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !finished.Load() {
				t.Fatal("Close returned while a handler was still running; the connection went away under it")
			}
		})
	}
}

// A handler still running when Close's context ends is a drain that did not
// finish: Close reports it rather than returning nil.
func TestBusClose_ReportsADrainThatTimedOut(t *testing.T) {
	for name, mk := range closeDrainBuses(t, "closedrain-timeout") {
		t.Run(name, func(t *testing.T) {
			bus := mk(t)
			release := make(chan struct{})
			defer close(release)
			subscribeBlocking(t, bus, 0, release)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			err := bus.Close(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close = %v, want an error wrapping context.DeadlineExceeded", err)
			}
			if again := bus.Close(context.Background()); again != nil {
				t.Errorf("second Close = %v, want nil (idempotent)", again)
			}
		})
	}
}
