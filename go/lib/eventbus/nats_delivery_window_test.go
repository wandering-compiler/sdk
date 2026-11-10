package eventbus_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// consumerInfos lists the JetStream consumers of one channel stream.
func consumerInfos(t *testing.T, url, stream string) []*jetstream.ConsumerInfo {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := js.Stream(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	var out []*jetstream.ConsumerInfo
	lst := st.ListConsumers(context.Background())
	for ci := range lst.Info() {
		out = append(out, ci)
	}
	if err := lst.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A subscription with a narrow topic filter reads only its own subjects.
//
// Every consumer used to filter on `<channel>.>`, whatever the subscription
// asked for: a plugin subscription on one literal topic received — and Acked,
// one by one — every message on the channel, and N subscriptions on a channel
// were N full deliveries of it. The filter is now pushed down to the broker
// whenever it can be expressed as subjects.
func TestNatsBus_NarrowFilterReadsOnlyItsSubjects_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: url, DurablePrefix: "narrow"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	sub, err := bus.Subscriber(context.Background(), "ch")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]*atomic.Int32{}
	for _, filter := range []string{"a.only", "a.*", "b.**"} {
		n := &atomic.Int32{}
		counts[filter] = n
		if err := sub.Subscribe(context.Background(), filter, func(context.Context, string, []byte) error {
			n.Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, topic := range []string{"b.other", "b.other", "b", "c.x", "c.y", "a.only", "a.two"} {
		if err := bus.Dispatch(context.Background(), "ch", topic, wrapperspb.String(topic)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int32{"a.only": 1, "a.*": 2, "b.**": 3}
	waitFor(t, 5*time.Second, "every subscription received its messages", func() bool {
		for f, n := range want {
			if counts[f].Load() != n {
				return false
			}
		}
		return true
	})
	time.Sleep(300 * time.Millisecond)
	for _, ci := range consumerInfos(t, url, "ch") {
		var expect uint64
		switch ci.Name {
		case "narrow-ch-a_only":
			expect = 1
		case "narrow-ch-a_-s":
			expect = 2
		case "narrow-ch-b_-s-s":
			expect = 3
		default:
			t.Errorf("unexpected consumer %q", ci.Name)
			continue
		}
		if ci.Delivered.Consumer != expect {
			t.Errorf("consumer %q (filters %q %q) was delivered %d messages, want %d — it reads more of the channel than its filter",
				ci.Name, ci.Config.FilterSubject, ci.Config.FilterSubjects, ci.Delivered.Consumer, expect)
		}
	}
}

// A delivery the client holds but has not begun to handle must not age past
// AckWait.
//
// The consume loop used to prefetch up to 500 messages and hand them to the
// handler one by one, while the server's AckWait for each ran from the moment
// it was delivered to the client. One slow handler pushed every message
// queued behind it past AckWait: the server redelivered them and they were
// handled twice although none had failed.
func TestNatsBus_QueuedDeliveriesDoNotOverrunAckWait_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: url, DurablePrefix: "window", DefaultAckWait: 500 * time.Millisecond, DefaultMaxDeliver: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	// Published before Subscribe, so the first pull could take them all.
	const n = 6
	for i := 0; i < n; i++ {
		if err := bus.Dispatch(context.Background(), "ch", "t", wrapperspb.String(strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	sub, err := bus.Subscriber(context.Background(), "ch")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	runs := map[string]int{}
	var calls atomic.Int32
	if err := sub.Subscribe(context.Background(), "**", func(_ context.Context, _ string, raw []byte) error {
		var v wrapperspb.StringValue
		if err := proto.Unmarshal(raw, &v); err != nil {
			return err
		}
		mu.Lock()
		runs[v.GetValue()]++
		mu.Unlock()
		if calls.Add(1) == 1 {
			time.Sleep(1200 * time.Millisecond) // one slow handler, well past AckWait
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "every message handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(runs) == n
	})
	time.Sleep(2 * time.Second) // a redelivery would land well within this
	mu.Lock()
	defer mu.Unlock()
	// The slow message overran its own AckWait, which is the attempt's
	// timeout: its redelivery is the documented behaviour. Every message
	// queued behind it must run exactly once.
	for i := 1; i < n; i++ {
		if got := runs[strconv.Itoa(i)]; got != 1 {
			t.Errorf("message %d ran %d times — a delivery queued behind a slow handler overran AckWait and ran again although it never failed", i, got)
		}
	}
}
