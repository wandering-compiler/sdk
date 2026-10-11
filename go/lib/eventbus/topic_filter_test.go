package eventbus_test

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// topicRecorder collects the topics one handler was invoked with.
type topicRecorder struct {
	mu     sync.Mutex
	topics []string
}

func (r *topicRecorder) handle(_ context.Context, topic string, _ []byte) error {
	r.mu.Lock()
	r.topics = append(r.topics, topic)
	r.mu.Unlock()
	return nil
}

func (r *topicRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.topics...)
	sort.Strings(out)
	return out
}

// assertTopicFilterHonoured subscribes three handlers with different topic
// filters on one channel, emits one event per topic, and checks each
// handler saw exactly the topics its filter matches — the contract the
// memory bus has always kept and every adapter must keep, because a
// hand-written subscriber relies on it rather than re-filtering.
func assertTopicFilterHonoured(t *testing.T, bus interface {
	eventbus.Dispatcher
	eventbus.SubscriberFactory
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sub, err := bus.Subscriber(ctx, "filtered")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	users, orders, all := &topicRecorder{}, &topicRecorder{}, &topicRecorder{}
	for filter, rec := range map[string]*topicRecorder{"user.*": users, "order.**": orders, "**": all} {
		if err := sub.Subscribe(ctx, filter, rec.handle); err != nil {
			t.Fatalf("Subscribe(%q): %v", filter, err)
		}
	}
	for _, topic := range []string{"order.created", "user.created", "order.line.added", "user.profile.updated"} {
		if err := bus.Dispatch(ctx, "filtered", topic, wrapperspb.String(topic)); err != nil {
			t.Fatalf("Dispatch(%q): %v", topic, err)
		}
	}

	want := map[string][]string{
		"user.*":   {"user.created"},
		"order.**": {"order.created", "order.line.added"},
		"**":       {"order.created", "order.line.added", "user.created", "user.profile.updated"},
	}
	recs := map[string]*topicRecorder{"user.*": users, "order.**": orders, "**": all}
	waitFor(t, 10*time.Second, "catch-all subscriber received every event", func() bool {
		return len(all.got()) >= 4 && len(orders.got()) >= 2 && len(users.got()) >= 1
	})
	time.Sleep(300 * time.Millisecond) // let any stray delivery land
	for filter, rec := range recs {
		if got := strings.Join(rec.got(), ","); got != strings.Join(want[filter], ",") {
			t.Errorf("handler subscribed with %q received [%s], want [%s]", filter, got, strings.Join(want[filter], ","))
		}
	}
}

func TestTopicFilter_Memory(t *testing.T) {
	assertTopicFilterHonoured(t, eventbus.NewMemoryBus())
}

func TestTopicFilter_Nats_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: startEmbeddedNATS(t), DurablePrefix: "filter"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	assertTopicFilterHonoured(t, bus)
}

func TestTopicFilter_Redis_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: startEmbeddedRedis(t), GroupPrefix: "filter"})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	assertTopicFilterHonoured(t, bus)
}
