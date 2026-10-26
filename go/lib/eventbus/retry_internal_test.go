package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// retryBrokerOp is the bounded retry that rides out a transient broker
// failure during DLQ routing. These cases pin its contract:
// succeed-on-retry, exhaust-and-return-last-error, and ctx-cancel-abort.
func TestRetryBrokerOp_SucceedsAfterTransientFailure(t *testing.T) {
	calls := 0
	transient := errors.New("broker hiccup")
	err := retryBrokerOp(context.Background(), 3, time.Microsecond, func() error {
		calls++
		if calls < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want success once the op recovers, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts (2 failures then success), got %d", calls)
	}
}

func TestRetryBrokerOp_ExhaustsAndReturnsLastError(t *testing.T) {
	calls := 0
	last := errors.New("still down")
	err := retryBrokerOp(context.Background(), 3, time.Microsecond, func() error {
		calls++
		return last
	})
	if !errors.Is(err, last) {
		t.Fatalf("want the last attempt's error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want exactly 3 attempts before giving up, got %d", calls)
	}
}

func TestRetryBrokerOp_CancelledContextAbortsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryBrokerOp(ctx, 5, time.Hour, func() error {
		calls++
		return errors.New("fail")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled once ctx is done, got %v", err)
	}
	// One attempt runs, then the cancelled ctx aborts the backoff wait
	// instead of blocking for the (1h) backoff.
	if calls != 1 {
		t.Fatalf("want the retry to abort after the first failure, got %d calls", calls)
	}
}

// A topic's retry narrows its channel's: a lower cap and its own
// backoff; a higher cap than the consumer's is the consumer's; a topic
// without an entry retries as the channel.
func TestResolveTopicRetry(t *testing.T) {
	overrides := map[string]ChannelRetry{
		"events": {MaxDeliver: 5, AckWait: 30 * time.Second, Backoff: BackoffExponential, Topics: map[string]TopicRetry{
			"probe.poison": {MaxDeliver: 3, AckWait: time.Second, Backoff: BackoffConstant},
			"probe.greedy": {MaxDeliver: 9},
		}},
	}
	cases := []struct {
		channel, topic string
		want           retryPlan
	}{
		{"events", "probe.poison", retryPlan{maxDeliver: 3, base: time.Second, backoff: BackoffConstant}},
		{"events", "probe.greedy", retryPlan{maxDeliver: 5, base: 30 * time.Second, backoff: BackoffExponential}},
		{"events", "user.created", retryPlan{maxDeliver: 5, base: 30 * time.Second, backoff: BackoffExponential}},
		{"other", "probe.poison", retryPlan{maxDeliver: 3, base: 30 * time.Second}},
	}
	for _, c := range cases {
		if got := resolveTopicRetry(overrides, c.channel, c.topic, 3, 30*time.Second); got != c.want {
			t.Errorf("%s/%s: got %+v, want %+v", c.channel, c.topic, got, c.want)
		}
	}
	if !hasTopicRetries(overrides, "events", DeliveryQueue) || hasTopicRetries(overrides, "other", DeliveryQueue) {
		t.Error("hasTopicRetries: events has topic retries, other has none")
	}
	if hasTopicRetries(map[string]ChannelRetry{"plain": {MaxDeliver: 4}}, "plain", DeliveryQueue) {
		t.Error("hasTopicRetries: a channel with neither backoff nor topics needs no plan")
	}
	spaced := map[string]ChannelRetry{"spaced": {Backoff: BackoffExponential}}
	if !hasTopicRetries(spaced, "spaced", DeliveryQueue) || hasTopicRetries(spaced, "spaced", DeliveryFanOut) {
		t.Error("hasTopicRetries: a backoff needs a plan on a queue, not on a fan-out surface")
	}
	if p := resolveTopicRetry(overrides, "events", "probe.poison", 3, time.Second).forMode(DeliveryFanOut); p.backoff != BackoffNone || p.maxDeliver != 3 {
		t.Errorf("fan-out plan: got %+v, want the topic's cap without backoff", p)
	}
}

// The delay after the attempt-th failed delivery, per the w17
// RetryPolicy's Backoff: CONSTANT base, LINEAR attempt×base,
// EXPONENTIAL 2^attempt×base, all capped at MaxBackoff.
func TestRetryPlanDelay(t *testing.T) {
	s := time.Second
	cases := []struct {
		backoff Backoff
		attempt int
		want    time.Duration
	}{
		{BackoffNone, 1, 0},
		{BackoffConstant, 1, s},
		{BackoffConstant, 4, s},
		{BackoffLinear, 1, s},
		{BackoffLinear, 3, 3 * s},
		{BackoffExponential, 1, 2 * s},
		{BackoffExponential, 3, 8 * s},
		{BackoffExponential, 40, MaxBackoff},
		{BackoffLinear, 1 << 40, MaxBackoff},
	}
	for _, c := range cases {
		if got := (retryPlan{base: s, backoff: c.backoff}).delay(c.attempt); got != c.want {
			t.Errorf("backoff %d attempt %d: got %v, want %v", c.backoff, c.attempt, got, c.want)
		}
	}
}

// The next XPENDING page starts just after the last id of the previous one.
func TestNextStreamID(t *testing.T) {
	cases := map[string]string{
		"1700000000000-0":                    "1700000000000-1",
		"1700000000000-41":                   "1700000000000-42",
		"1700000000000-18446744073709551615": "1700000000001-0",
	}
	for in, want := range cases {
		if got, ok := nextStreamID(in); !ok || got != want {
			t.Errorf("nextStreamID(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := nextStreamID("garbage"); ok {
		t.Error("nextStreamID accepted an id without a sequence")
	}
}
