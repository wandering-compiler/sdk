package eventbus_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// assertReservedTopicNotInCatchAll is the regression guard for the reserved
// `w17.*` topics on a domain channel.
//
// EmitInvalidate publishes a bare w17.InvalidateEvent on the domain's channel.
// The generated subscriber bundle subscribes that channel with `**` and decodes
// every delivery into the domain's envelope, which an InvalidateEvent is not:
// each invalidate failed to decode, was Nak'd to MaxDeliver, dead-lettered,
// and reported four times. A wildcard therefore never matches a reserved
// topic: only a filter naming `w17` literally, or [eventbus.EveryTopic] (the
// /w17-events hub), receives it.
func assertReservedTopicNotInCatchAll(t *testing.T, bus interface {
	eventbus.Dispatcher
	eventbus.SubscriberFactory
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sub, err := bus.Subscriber(ctx, "tasks")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	recs := map[string]*topicRecorder{}
	for _, filter := range []string{"**", "*.invalidate", "w17.invalidate", "w17.**", eventbus.EveryTopic} {
		rec := &topicRecorder{}
		recs[filter] = rec
		if err := sub.Subscribe(ctx, filter, rec.handle); err != nil {
			t.Fatalf("Subscribe(%q): %v", filter, err)
		}
	}
	if err := bus.Dispatch(ctx, "tasks", "task.created", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := eventbus.EmitInvalidate(ctx, bus, "tasks", "tasks", "Task", nil); err != nil {
		t.Fatalf("EmitInvalidate: %v", err)
	}
	want := map[string][]string{
		"**":                {"task.created"},
		"*.invalidate":      nil,
		"w17.invalidate":    {eventbus.W17InvalidateTopic},
		"w17.**":            {eventbus.W17InvalidateTopic},
		eventbus.EveryTopic: {"task.created", eventbus.W17InvalidateTopic},
	}
	waitFor(t, 10*time.Second, "every-topic subscriber received both events", func() bool {
		return len(recs[eventbus.EveryTopic].got()) >= 2 && len(recs["w17.invalidate"].got()) >= 1
	})
	time.Sleep(300 * time.Millisecond) // let any stray delivery land
	for filter, rec := range recs {
		if got, w := strings.Join(rec.got(), ","), strings.Join(want[filter], ","); got != w {
			t.Errorf("handler subscribed with %q received [%s], want [%s]", filter, got, w)
		}
	}
}

func TestReservedTopic_Memory(t *testing.T) {
	assertReservedTopicNotInCatchAll(t, eventbus.NewMemoryBus())
}

func TestReservedTopic_Nats_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: startEmbeddedNATS(t), DurablePrefix: "reserved"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	assertReservedTopicNotInCatchAll(t, bus)
}

func TestReservedTopic_Redis_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: startEmbeddedRedis(t), GroupPrefix: "reserved"})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	assertReservedTopicNotInCatchAll(t, bus)
}

func TestMatchGlob_ReservedTopic(t *testing.T) {
	for _, tc := range []struct {
		filter, topic string
		want          bool
	}{
		{"**", "w17.invalidate", false},
		{"*.*", "w17.invalidate", false},
		{"*.invalidate", "w17.invalidate", false},
		{"**.invalidate", "w17.invalidate", false},
		{"w17.*", "w17.invalidate", true},
		{"w17.invalidate", "w17.invalidate", true},
		{eventbus.EveryTopic, "w17.invalidate", true},
		{eventbus.EveryTopic, "user.created", true},
		{"**", "user.w17.created", true},
		{"**", "w17x.created", true},
	} {
		if got := eventbus.MatchGlob(tc.filter, tc.topic); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}
