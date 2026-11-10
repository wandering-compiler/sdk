package eventbus_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A handler that panics on every delivery is treated like one that fails:
// the panic is recovered (the process survives and the panic is reported),
// the message is retried up to MaxDeliver and then dead-lettered, on both
// broker transports.
func TestHandlerPanic_IsRetriedThenDeadLettered(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for _, tc := range []struct {
		name string
		// open returns the bus and a reader of the dead-letter stream's
		// length, so the test reads the payload's arrival there back
		// rather than trusting OnDrop alone.
		open func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory, func() int)
	}{
		{"nats", func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory, func() int) {
			url := startEmbeddedNATS(t)
			b, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: url, DurablePrefix: "panics",
				DefaultMaxDeliver: 2, DefaultAckWait: 300 * time.Millisecond, Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close(timeout(t, 5*time.Second)) })
			nc, err := natsgo.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			js, err := jetstream.New(nc)
			if err != nil {
				t.Fatal(err)
			}
			return b, b, func() int {
				st, err := js.Stream(context.Background(), "default_dlq")
				if err != nil {
					return 0
				}
				info, err := st.Info(context.Background())
				if err != nil {
					return 0
				}
				return int(info.State.Msgs)
			}
		}},
		{"redis", func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory, func() int) {
			dsn := startEmbeddedRedis(t)
			b, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: dsn, GroupPrefix: "panics",
				DefaultMaxDeliver: 2, DefaultAckWait: 200 * time.Millisecond, Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close(timeout(t, 5*time.Second)) })
			raw := rawRedisClient(t, dsn)
			return b, b, func() int {
				n, _ := raw.XLen(context.Background(), "default.dlq").Result()
				return int(n)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			bus, subs, dlqLen := tc.open(t, obs)
			sub, err := subs.Subscriber(context.Background(), "default")
			if err != nil {
				t.Fatal(err)
			}
			var attempts atomic.Int32
			if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
				attempts.Add(1)
				panic("handler exploded on this payload")
			}); err != nil {
				t.Fatal(err)
			}
			if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("poison")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 15*time.Second, "OnDrop after the panicking handler's last attempt", func() bool {
				_, _, _, _, drops := obs.snapshots()
				return drops >= 1
			})
			if n := attempts.Load(); n < 2 {
				t.Errorf("the panicking handler ran %d time(s), want it retried up to MaxDeliver (2)", n)
			}
			if _, _, _, failures, _ := obs.snapshots(); failures < 2 {
				t.Errorf("OnDeliverFailure fired %d times, want once per panicking attempt", failures)
			}
			if n := dlqLen(); n != 1 {
				t.Errorf("the dead-letter stream holds %d messages after OnDrop, want the poison message once", n)
			}
		})
	}
}

// The in-memory transport has no retry and no dead-letter stream (I4.2): a
// panicking handler is recovered and reported once, and the process and the
// bus survive it; the event is not redelivered.
func TestHandlerPanic_MemoryTransportRecoversAndReportsOnly(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})
	sub, _ := bus.Subscriber(context.Background(), "default")
	var attempts atomic.Int32
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		attempts.Add(1)
		panic("handler exploded on this payload")
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("poison")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "OnDeliverFailure for the panic", func() bool {
		_, _, _, failures, _ := obs.snapshots()
		return failures == 1
	})
	time.Sleep(100 * time.Millisecond)
	if n := attempts.Load(); n != 1 {
		t.Errorf("the panicking handler ran %d times on the memory transport, want once (no retry)", n)
	}
	if _, _, _, _, drops := obs.snapshots(); drops != 0 {
		t.Errorf("OnDrop fired %d times; the memory transport has no dead-letter stream", drops)
	}
	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("next")); err != nil {
		t.Errorf("the bus refused the next dispatch after a handler panic: %v", err)
	}
}
