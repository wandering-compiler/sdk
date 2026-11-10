package eventbus_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// An entry another writer appended without a `topic` field is not an event
// envelope: even the catch-all `**`, which matches the empty topic, must not
// hand it to the handler. It is settled (acknowledged) and reported, so it
// neither loops through the claim loop nor disappears without a trace.
func TestRedisBus_EntryWithoutTopicIsSettledUnhandled_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:            dsn,
		GroupPrefix:    "notopic-subscribers",
		DefaultAckWait: 200 * time.Millisecond,
		Observer:       obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var emptyTopic atomic.Int32
	got := make(chan string, 4)
	if err := sub.Subscribe(context.Background(), "**", func(_ context.Context, topic string, _ []byte) error {
		if topic == "" {
			emptyTopic.Add(1)
		}
		got <- topic
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	raw := rawRedisClient(t, dsn)
	if err := raw.XAdd(context.Background(), &goredis.XAddArgs{
		Stream: "default",
		Values: map[string]any{"payload": "not-an-envelope"},
	}).Err(); err != nil {
		t.Fatalf("XADD: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case topic := <-got:
		if topic != "user.created" {
			t.Fatalf("handler got topic %q first, want user.created", topic)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the envelope after the topic-less entry never arrived")
	}
	// Give the claim loop a few ticks: an unsettled entry would come back.
	time.Sleep(800 * time.Millisecond)
	if n := emptyTopic.Load(); n != 0 {
		t.Fatalf("handler ran %d times for an entry without a topic field", n)
	}
	waitFor(t, 3*time.Second, "the topic-less entry reported", func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		for _, r := range obs.deliverFailure {
			if r.Topic == "" && strings.Contains(r.Err, "topic") {
				return true
			}
		}
		return false
	})
	pending, err := raw.XPending(context.Background(), "default", "notopic-subscribers-default").Result()
	if err != nil {
		t.Fatalf("XPENDING: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("%d entries still pending; the topic-less entry was not settled", pending.Count)
	}
}
