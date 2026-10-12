package eventbus_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
		open func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory)
	}{
		{"nats", func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory) {
			b, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: startEmbeddedNATS(t), DurablePrefix: "panics",
				DefaultMaxDeliver: 2, DefaultAckWait: 300 * time.Millisecond, Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close(timeout(t, 5*time.Second)) })
			return b, b
		}},
		{"redis", func(t *testing.T, obs eventbus.Observer) (eventbus.Dispatcher, eventbus.SubscriberFactory) {
			b, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: startEmbeddedRedis(t), GroupPrefix: "panics",
				DefaultMaxDeliver: 2, DefaultAckWait: 200 * time.Millisecond, Observer: obs})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close(timeout(t, 5*time.Second)) })
			return b, b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			bus, subs := tc.open(t, obs)
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
		})
	}
}
