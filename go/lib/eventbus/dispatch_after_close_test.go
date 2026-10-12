package eventbus_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// No event vanishes without a trace on a closed bus: on the broker
// transports a Dispatch after Close returns an error and reports
// OnEmitFailure, promptly (the in-memory bus after drain:
// TestMemoryBus_DispatchAfterDrainIsRefused).
func TestDispatchAfterClose_IsAnErrorOnEveryTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, obs eventbus.Observer) interface {
			eventbus.Dispatcher
			Close(context.Context) error
		}
	}{
		{"nats", func(t *testing.T, obs eventbus.Observer) interface {
			eventbus.Dispatcher
			Close(context.Context) error
		} {
			b, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: startEmbeddedNATS(t), DurablePrefix: "closed", Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"redis", func(t *testing.T, obs eventbus.Observer) interface {
			eventbus.Dispatcher
			Close(context.Context) error
		} {
			b, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: startEmbeddedRedis(t), GroupPrefix: "closed", Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			bus := tc.open(t, obs)
			if err := bus.Close(timeout(t, 5*time.Second)); err != nil {
				t.Fatalf("Close: %v", err)
			}
			start := time.Now()
			if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("x")); err == nil {
				t.Fatal("Dispatch on a closed bus returned nil")
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("Dispatch on a closed bus took %v to fail", took.Round(time.Millisecond))
			}
			if _, emitFailures, _, _, _ := obs.snapshots(); emitFailures != 1 {
				t.Errorf("OnEmitFailure fired %d times, want 1", emitFailures)
			}
		})
	}
}
