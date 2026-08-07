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
// Per-event retry plumbing through the Subscriber interface is
// strictly more granular than per-channel; v1 rolls events
// up to channel-level max via codegen (see
// the eventbus codegen),
// matching the cascade's intent within the broker's
// per-consumer retry model. Per-event-strict (requires
// delivery-count metadata reaching the handler + dispatch-
// side capping) is parked.
type ChannelRetry struct {
	// MaxDeliver caps redelivery attempts. Zero leaves the
	// bus's Default in place.
	MaxDeliver int

	// AckWait is the per-attempt timeout. Zero leaves the
	// bus's Default in place.
	AckWait time.Duration
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
