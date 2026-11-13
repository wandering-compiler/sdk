package eventbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// An entry left pending comes back once it has been idle for AckWait, and the
// claim loop's tick decides how much later than that. Ticking once per AckWait
// made it up to twice AckWait: an entry that turned due just after a tick
// waited a whole period for the next. It is how a message handed to a
// connection that died before it arrived came back late enough to miss the
// fan-out reconnect test's budget. The failure here lands a fifth of AckWait
// after a tick, so the old loop redelivered at about 1.8 × AckWait.
func TestRedisBus_PendingEntryReturnsSoonAfterAckWait_Embedded(t *testing.T) {
	const ackWait = time.Second
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:            startEmbeddedRedis(t),
		GroupPrefix:    "claimtick",
		DefaultAckWait: ackWait,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(context.Background(), "tick")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var deliveries []time.Time
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		mu.Lock()
		defer mu.Unlock()
		deliveries = append(deliveries, time.Now())
		if len(deliveries) == 1 {
			return errors.New("first delivery fails")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(ackWait / 5)
	if err := bus.Dispatch(context.Background(), "tick", "a.b", wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*ackWait, "the failed entry redelivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(deliveries) >= 2
	})
	mu.Lock()
	gap := deliveries[1].Sub(deliveries[0])
	mu.Unlock()
	if limit := ackWait * 3 / 2; gap > limit {
		t.Errorf("redelivered %v after the failure, want within %v (AckWait %v)", gap, limit, ackWait)
	}
}
