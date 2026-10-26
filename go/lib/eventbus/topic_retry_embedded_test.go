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

// attemptLog records when each topic's deliveries arrived.
type attemptLog struct {
	mu sync.Mutex
	at map[string][]time.Time
}

func (l *attemptLog) failing(_ context.Context, topic string, _ []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.at == nil {
		l.at = map[string][]time.Time{}
	}
	l.at[topic] = append(l.at[topic], time.Now())
	return errors.New("always fails")
}

func (l *attemptLog) of(topic string) []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.at[topic]...)
}

// minGap is the shortest interval between successive attempts.
func minGap(at []time.Time) time.Duration {
	gap := time.Duration(1<<63 - 1)
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < gap {
			gap = d
		}
	}
	return gap
}

// topicRetries is a channel that allows 6 deliveries redelivered at
// once, and one topic on it that allows 3 spaced 300ms apart — the
// consumer's probe: an event's own retry under its channel's.
func topicRetries() map[string]eventbus.ChannelRetry {
	return map[string]eventbus.ChannelRetry{
		"default": {MaxDeliver: 6, Topics: map[string]eventbus.TopicRetry{
			"probe.poison": {MaxDeliver: 3, AckWait: 300 * time.Millisecond, Backoff: eventbus.BackoffConstant},
		}},
	}
}

// INVARIANT: on NATS an event's own retry applies under its channel's —
// its lower cap ends it in the DLQ after 3 deliveries while a sibling
// topic gets the channel's 6, and its backoff spaces the redeliveries.
func TestNatsBus_TopicRetry_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:            startEmbeddedNATS(t),
		DurablePrefix:  "topicretry-subscribers",
		DefaultAckWait: 10 * time.Second,
		ChannelRetries: topicRetries(),
		Observer:       obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	log := &attemptLog{}
	_ = sub.Subscribe(context.Background(), "**", log.failing)

	_ = bus.Dispatch(context.Background(), "default", "probe.poison", wrapperspb.String("doomed"))
	_ = bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("doomed"))

	waitFor(t, 10*time.Second, "both topics dead-lettered", func() bool {
		_, poison := topicOutcomes(obs, "probe.poison")
		_, other := topicOutcomes(obs, "user.created")
		return poison >= 1 && other >= 1
	})
	time.Sleep(500 * time.Millisecond)

	if got := len(log.of("probe.poison")); got != 3 {
		t.Errorf("probe.poison delivered %d times, want its own max_deliver 3", got)
	}
	if got := len(log.of("user.created")); got != 6 {
		t.Errorf("user.created delivered %d times, want the channel's max_deliver 6", got)
	}
	if gap := minGap(log.of("probe.poison")); gap < 250*time.Millisecond {
		t.Errorf("probe.poison redelivered %v after a failure, want its constant 300ms backoff", gap)
	}
}

// INVARIANT: on Redis Streams the same: the topic's lower cap, the
// channel's for a sibling. Its backoff cannot space retries closer than
// the channel's ack wait (a failed delivery looks like one still
// running elsewhere), so this one is set above it and must hold.
func TestRedisBus_TopicRetry_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	retries := topicRetries()
	ch := retries["default"]
	ch.Topics["probe.poison"] = eventbus.TopicRetry{MaxDeliver: 3, AckWait: 400 * time.Millisecond, Backoff: eventbus.BackoffConstant}
	retries["default"] = ch
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:            dsn,
		GroupPrefix:    "topicretry-subscribers",
		DefaultAckWait: 100 * time.Millisecond,
		ChannelRetries: retries,
		Observer:       obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	log := &attemptLog{}
	_ = sub.Subscribe(context.Background(), "**", log.failing)

	_ = bus.Dispatch(context.Background(), "default", "probe.poison", wrapperspb.String("doomed"))
	_ = bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("doomed"))

	waitFor(t, 10*time.Second, "both topics dead-lettered", func() bool {
		_, poison := topicOutcomes(obs, "probe.poison")
		_, other := topicOutcomes(obs, "user.created")
		return poison >= 1 && other >= 1
	})
	_ = sub.Drain(timeout(t, 3*time.Second))

	if got := len(log.of("probe.poison")); got != 3 {
		t.Errorf("probe.poison delivered %d times, want its own max_deliver 3", got)
	}
	if got := len(log.of("user.created")); got != 6 {
		t.Errorf("user.created delivered %d times, want the channel's max_deliver 6", got)
	}
	if gap := minGap(log.of("probe.poison")); gap < 350*time.Millisecond {
		t.Errorf("probe.poison redelivered %v after a failure, want its constant 400ms backoff", gap)
	}
}

// INVARIANT: on Redis Streams a backoff counts from the failure, as on
// NATS. The entry's idle time counts from its delivery, so a handler that
// ran for most of the delay used to be redelivered almost at once.
func TestRedisBus_BackoffCountsFromTheFailure_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:            startEmbeddedRedis(t),
		GroupPrefix:    "backofffromfailure-subscribers",
		DefaultAckWait: 100 * time.Millisecond,
		ChannelRetries: map[string]eventbus.ChannelRetry{
			"default": {MaxDeliver: 2, Backoff: eventbus.BackoffConstant, AckWait: 400 * time.Millisecond},
		},
		Observer: obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	var mu sync.Mutex
	var started, failed []time.Time
	_ = sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		mu.Lock()
		started = append(started, time.Now())
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		failed = append(failed, time.Now())
		mu.Unlock()
		return errors.New("slow, then fails")
	})
	_ = bus.Dispatch(context.Background(), "default", "slow", wrapperspb.String("doomed"))

	waitFor(t, 10*time.Second, "dead-lettered", func() bool {
		_, drops := topicOutcomes(obs, "slow")
		return drops >= 1
	})
	_ = sub.Drain(timeout(t, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 {
		t.Fatalf("delivered %d times, want 2", len(started))
	}
	if gap := started[1].Sub(failed[0]); gap < 350*time.Millisecond {
		t.Errorf("redelivered %v after the failure, want the 400ms backoff from it", gap)
	}
}

// INVARIANT: entries waiting out a long backoff do not hide due ones
// behind them. More than one XPENDING page of them sits ahead of an entry
// whose short backoff is due; it is reclaimed and dead-lettered all the
// same.
func TestRedisBus_BackoffDoesNotHideDueEntries_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:            startEmbeddedRedis(t),
		GroupPrefix:    "backoffwindow-subscribers",
		DefaultAckWait: 100 * time.Millisecond,
		ChannelRetries: map[string]eventbus.ChannelRetry{
			"default": {MaxDeliver: 5, Topics: map[string]eventbus.TopicRetry{
				"slow": {Backoff: eventbus.BackoffConstant, AckWait: time.Minute},
				"fast": {MaxDeliver: 2, Backoff: eventbus.BackoffConstant, AckWait: 200 * time.Millisecond},
			}},
		},
		ReadBatch: 200,
		Observer:  obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	for i := 0; i < 150; i++ {
		_ = bus.Dispatch(context.Background(), "default", "slow", wrapperspb.String("waits"))
	}
	_ = bus.Dispatch(context.Background(), "default", "fast", wrapperspb.String("due"))

	sub, _ := bus.Subscriber(context.Background(), "default")
	_ = sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		return errors.New("always fails")
	})

	waitFor(t, 10*time.Second, "the fast topic dead-lettered behind 150 waiting entries", func() bool {
		_, drops := topicOutcomes(obs, "fast")
		return drops >= 1
	})
}
