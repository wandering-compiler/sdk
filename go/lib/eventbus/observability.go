package eventbus

import "time"

// Observer is the cross-cutting metrics + logs surface every
// eventbus transport calls into at the boundaries of the emit
// and deliver pipelines. Implementations wire to a project's
// chosen metrics backend (Prometheus / OpenTelemetry /
// structured logger) — the bus stays decoupled from any
// specific backend.
//
// The five callbacks cover the success + failure axes on both
// sides of the broker plus an explicit drop hook for
// backpressure paths:
//
//	OnEmitSuccess     — Dispatch published the envelope.
//	OnEmitFailure     — Dispatch failed before publish (proto
//	                    marshal error, transport publish
//	                    error).
//	OnDeliverSuccess  — handler returned nil + ack succeeded.
//	OnDeliverFailure  — handler returned err (Nak'd / left in
//	                    PEL for redelivery).
//	OnDrop            — bounded buffer overflow (MemoryBus
//	                    MaxBlockTimeout exceeded) or DLQ
//	                    routing past MaxDeliver
//	                    (RedisBus claim loop).
//
// All callbacks run on the hot path; implementations should
// be O(1) — typically a counter increment or a structured-log
// line. Heavy work (histogram percentile compute, log
// flushing) belongs in a background goroutine fed via a
// buffered channel.
type Observer interface {
	OnEmitSuccess(channel, topic string)
	OnEmitFailure(channel, topic string, err error)
	OnDeliverSuccess(channel, topic string)
	OnDeliverFailure(channel, topic string, err error)
	OnDrop(channel, topic, reason string)
}

// NopObserver is the default Observer wired by every bus when
// the caller's Options leaves Observer nil — every callback
// is a silent no-op. Lets the bus call Observer methods
// unconditionally without a nil check on the hot path.
type NopObserver struct{}

func (NopObserver) OnEmitSuccess(string, string)           {}
func (NopObserver) OnEmitFailure(string, string, error)    {}
func (NopObserver) OnDeliverSuccess(string, string)        {}
func (NopObserver) OnDeliverFailure(string, string, error) {}
func (NopObserver) OnDrop(string, string, string)          {}

// ObserverWithLatency is an additive extension of Observer
// adding handler-side latency reporting. Implementations that
// satisfy it receive one OnDeliverComplete call per handler
// invocation — fired on BOTH success and failure paths so
// operators trace slow errors with the same fidelity as slow
// successes.
//
// The bus type-asserts the configured Observer at delivery
// time; impls that only satisfy Observer are unaffected
// (no extra cost, no contract change). Backward-compat with
// every existing impl is guaranteed via the embedded base
// interface.
type ObserverWithLatency interface {
	Observer
	OnDeliverComplete(channel, topic string, duration time.Duration)
}

// reportDeliverLatency forwards a delivery's elapsed time to
// the configured Observer when it implements
// ObserverWithLatency, otherwise no-ops. Lets transports call
// this unconditionally on the hot path without spreading the
// type assertion across every call site.
func reportDeliverLatency(obs Observer, channel, topic string, duration time.Duration) {
	if olat, ok := obs.(ObserverWithLatency); ok {
		olat.OnDeliverComplete(channel, topic, duration)
	}
}

// Drop reason strings — small set of canonical labels every
// transport uses for OnDrop's `reason` arg. Consumers
// switching on the reason can ship a stable contract; new
// reasons are additive.
const (
	// DropReasonBufferFull — emit dropped because the
	// channel's bounded buffer was saturated past the bus's
	// BlockTimeout (MemoryBus E6 path).
	DropReasonBufferFull = "buffer_full"

	// DropReasonMaxDeliverExceeded — message routed to DLQ
	// because the PEL delivery counter exceeded MaxDeliver
	// (RedisBus claim-loop path).
	DropReasonMaxDeliverExceeded = "max_deliver_exceeded"
)
