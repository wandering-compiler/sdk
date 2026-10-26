package eventbus

import "time"

// ChannelRetry overrides the bus-level Default{MaxDeliver,AckWait}
// for one specific channel. Set via Options.ChannelRetries (a
// `map[string]ChannelRetry` keyed by channel name).
//
// Zero-value fields fall through to the bus's Default* —
// presence-aware merge same as the parser's tier cascade. So
// a ChannelRetry with `MaxDeliver: 10, AckWait: 0` keeps the
// bus AckWait default but raises MaxDeliver for this channel.
//
// MaxDeliver and AckWait configure the broker consumer, which is one
// per channel: they are the ceiling every topic on the channel lives
// under. Topics narrows that per topic — a lower MaxDeliver, its own
// backoff — and is applied by the bus as each delivery is settled
// (see TopicRetry).
type ChannelRetry struct {
	// MaxDeliver caps redelivery attempts. Zero leaves the
	// bus's Default in place.
	MaxDeliver int

	// AckWait is the per-attempt timeout. Zero leaves the
	// bus's Default in place.
	AckWait time.Duration

	// Backoff spaces the redeliveries of a failed message on this
	// channel (see Backoff). Zero (BackoffNone) redelivers as the
	// broker would on its own.
	Backoff Backoff

	// Topics carries the topics whose retry differs from the
	// channel's, keyed by the exact topic. A topic missing from the
	// map retries as the channel does.
	Topics map[string]TopicRetry
}

// TopicRetry is one topic's retry within its channel's ChannelRetry.
// A zero field takes the channel's value.
type TopicRetry struct {
	// MaxDeliver caps this topic's delivery attempts. It can only
	// lower the channel's: the broker consumer stops at the
	// channel's MaxDeliver whatever a topic says, so a higher value
	// is treated as the channel's.
	MaxDeliver int

	// AckWait is the base the topic's Backoff multiplies. The
	// attempt timeout itself stays the channel's — the broker
	// consumer has one — so this sets the spacing of retries, not
	// how long one attempt may run.
	AckWait time.Duration

	// Backoff spaces this topic's redeliveries.
	Backoff Backoff
}

// Backoff is the shape of the delay between a failed delivery and its
// redelivery, after the w17 RetryPolicy's Backoff.
type Backoff int

const (
	// BackoffNone redelivers without a delay of the bus's own: NATS
	// at once, Redis Streams once the entry has been idle AckWait.
	// It is what a bus did before Backoff existed.
	BackoffNone Backoff = iota
	// BackoffConstant waits AckWait before every redelivery.
	BackoffConstant
	// BackoffLinear waits attempt × AckWait.
	BackoffLinear
	// BackoffExponential waits 2^attempt × AckWait.
	BackoffExponential
)

// MaxBackoff caps every computed redelivery delay.
const MaxBackoff = 5 * time.Minute

// retryPlan is what one delivery of one topic retries under.
type retryPlan struct {
	maxDeliver int
	base       time.Duration
	backoff    Backoff
}

// resolveChannelRetry returns the effective (MaxDeliver, AckWait) for
// channel: the per-channel override from overrides when present, else
// the supplied bus-level defaults. A zero override field falls through
// to its default (presence-aware merge — see ChannelRetry). Shared by
// the NATS and Redis adapters so the override cascade is defined once.
func resolveChannelRetry(overrides map[string]ChannelRetry, channel string, defMaxDeliver int, defAckWait time.Duration) (int, time.Duration) {
	maxDeliver, ackWait := defMaxDeliver, defAckWait
	if override, ok := overrides[channel]; ok {
		if override.MaxDeliver > 0 {
			maxDeliver = override.MaxDeliver
		}
		if override.AckWait > 0 {
			ackWait = override.AckWait
		}
	}
	return maxDeliver, ackWait
}

// resolveTopicRetry returns the plan a delivery of topic on channel
// retries under: the topic's entry over the channel's, over the bus
// defaults. A topic's MaxDeliver never exceeds the channel's.
func resolveTopicRetry(overrides map[string]ChannelRetry, channel, topic string, defMaxDeliver int, defAckWait time.Duration) retryPlan {
	maxDeliver, ackWait := resolveChannelRetry(overrides, channel, defMaxDeliver, defAckWait)
	plan := retryPlan{maxDeliver: maxDeliver, base: ackWait}
	ch, ok := overrides[channel]
	if !ok {
		return plan
	}
	plan.backoff = ch.Backoff
	t, ok := ch.Topics[topic]
	if !ok {
		return plan
	}
	if t.MaxDeliver > 0 && (plan.maxDeliver <= 0 || t.MaxDeliver < plan.maxDeliver) {
		plan.maxDeliver = t.MaxDeliver
	}
	if t.AckWait > 0 {
		plan.base = t.AckWait
	}
	if t.Backoff != BackoffNone {
		plan.backoff = t.Backoff
	}
	return plan
}

// hasTopicRetries reports whether channel's retry varies by topic or
// spaces its redeliveries — whether settling a delivery needs its plan.
func hasTopicRetries(overrides map[string]ChannelRetry, channel string) bool {
	ch, ok := overrides[channel]
	if !ok {
		return false
	}
	if ch.Backoff != BackoffNone {
		return true
	}
	return len(ch.Topics) > 0
}

// delay is how long to wait before redelivering after the attempt-th
// delivery (1-based) failed. Zero for BackoffNone.
func (p retryPlan) delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	var d time.Duration
	switch p.backoff {
	case BackoffConstant:
		d = p.base
	case BackoffLinear:
		if p.base > 0 && time.Duration(attempt) > MaxBackoff/p.base {
			return MaxBackoff
		}
		d = time.Duration(attempt) * p.base
	case BackoffExponential:
		d = p.base
		for i := 0; i < attempt; i++ {
			if d >= MaxBackoff {
				return MaxBackoff
			}
			d *= 2
		}
	default:
		return 0
	}
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}
