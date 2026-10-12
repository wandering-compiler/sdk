package eventbus

import "time"

// StreamRetention bounds what a broker keeps for one channel's stream. Both
// broker adapters apply it on every stream they create — the channel's own
// stream ([NatsBusOptions.Retention], [RedisBusOptions.Retention]) and its
// dead-letter stream (the DLQRetention fields) — so storage does not grow
// without bound unless someone asks for that explicitly.
//
// A message removed by retention is gone whether or not every consumer has
// processed it: size the limits above the longest outage a consumer is
// expected to catch up from.
type StreamRetention struct {
	// MaxAge removes messages older than this. Zero -> the default for the
	// stream kind ([DefaultStreamMaxAge], [DefaultDLQMaxAge]); negative ->
	// no age limit.
	MaxAge time.Duration

	// MaxMsgs keeps at most this many messages, removing the oldest first.
	// Zero or negative -> no count limit.
	MaxMsgs int64
}

const (
	// DefaultStreamMaxAge is how long a channel's stream keeps a message
	// when [StreamRetention.MaxAge] is left zero.
	DefaultStreamMaxAge = 7 * 24 * time.Hour

	// DefaultDLQMaxAge is how long a dead-letter stream keeps a message when
	// [StreamRetention.MaxAge] is left zero — longer than the main stream,
	// because a dead letter waits for a human.
	DefaultDLQMaxAge = 30 * 24 * time.Hour
)

// limits resolves the configured retention against the stream kind's
// default age. A zero result means "no limit" for that dimension.
func (r StreamRetention) limits(defaultAge time.Duration) (maxAge time.Duration, maxMsgs int64) {
	switch {
	case r.MaxAge == 0:
		maxAge = defaultAge
	case r.MaxAge > 0:
		maxAge = r.MaxAge
	}
	if r.MaxMsgs > 0 {
		maxMsgs = r.MaxMsgs
	}
	return maxAge, maxMsgs
}

// DefaultPublishTimeout bounds one emit on a broker adapter — the stream
// check, the publish and its acknowledgement together — when the adapter's
// PublishTimeout option is left zero. An emit runs after the database commit
// on the committing goroutine, so during a broker outage every emitted event
// adds up to this much to the request's latency before it is reported as a
// failed emit.
const DefaultPublishTimeout = 2 * time.Second
