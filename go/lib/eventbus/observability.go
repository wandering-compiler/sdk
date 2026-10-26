package eventbus

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

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
// A panic in a callback is recovered and reported by the bus; it
// never reaches the emitting caller or a consume loop.
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

// NopObserver is the EXPLICIT silent opt-out — every callback
// is a no-op. Lets the bus call Observer methods
// unconditionally without a nil check on the hot path.
//
// It is no longer the default. It used to be, and that made
// four written promises false at once: the emit spec says a
// failed emit is "logged + counted in observability metrics"
// and uses that as its reason for not failing the RPC, the
// dispatcher's header repeats it, and every emitted helper
// discards Dispatch's error with a comment saying failures are
// "observed via runtime metrics, not propagated". The only sink
// Dispatch has is OnEmitFailure, so with NopObserver a lost
// event left no trace anywhere — and the generated main gives a
// project no seam to supply an Observer of its own. A project that has measured that it cannot afford
// the logging still sets this explicitly.
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

// DefaultObserver is what a bus wires when the caller's Options
// leaves Observer nil: failures and drops leave a trace, successes
// do not.
//
// It is the floor, not a metrics backend. A project that wants
// counters supplies its own Observer; what this guarantees is that
// no deployment silently loses an event, which is the promise the
// emit pipeline's own design rests on — the emitted helper is
// allowed to discard Dispatch's error precisely BECAUSE something
// else is supposed to record it.
//
// Success callbacks stay silent on purpose. They are the hot path,
// one line per delivered event would drown a real log, and nothing
// promises them.
//
// Errors also go to observx so a project with Sentry / OTel wired
// sees them where it already looks; the log line is the part that
// works with no configuration at all.
type DefaultObserver struct{}

func (DefaultObserver) OnEmitSuccess(string, string) {}

func (DefaultObserver) OnEmitFailure(channel, topic string, err error) {
	reportBusFailure("emit", channel, topic, err)
}

func (DefaultObserver) OnDeliverSuccess(string, string) {}

func (DefaultObserver) OnDeliverFailure(channel, topic string, err error) {
	reportBusFailure("deliver", channel, topic, err)
}

// OnDrop says what happened to the message, not that something failed:
// "eventbus drop failed" read as a second fault when the drop was the
// outcome itself.
func (DefaultObserver) OnDrop(channel, topic, reason string) {
	what := reason
	switch reason {
	case DropReasonMaxDeliverExceeded:
		what = reason + ": every delivery failed; the message is in the channel's DLQ"
	case DropReasonBufferFull:
		what = reason + ": the channel's buffer stayed full; the message is lost"
	}
	err := fmt.Errorf("eventbus dropped a message: channel=%s topic=%s: %s", channel, topic, what)
	log.Print(err)
	observx.ReportError(context.Background(), err)
}

// reportBusFailure writes the one line an operator needs to identify
// WHICH event was lost — the channel and topic, not just that
// something failed.
func reportBusFailure(stage, channel, topic string, err error) {
	wrapped := fmt.Errorf("eventbus %s failed: channel=%s topic=%s: %w", stage, channel, topic, err)
	log.Print(wrapped)
	observx.ReportError(context.Background(), wrapped)
}

// guardedObserver recovers a panic raised by the Observer it wraps.
//
// Callbacks run inline: OnEmitSuccess / OnEmitFailure on the goroutine
// that called Dispatch (the emitting RPC), the delivery callbacks inside
// the consume loops and detached fan-out goroutines. A panicking metrics
// backend must cost the observation, never the emit — Dispatch has
// already published (or already decided to fail) by the time the
// callback runs, so propagating the panic would crash the caller over a
// counter and, on a broker, report an event that WAS sent as lost.
//
// The recovered panic is reported to observx and the log directly, not
// through the Observer, which is the thing that just failed.
type guardedObserver struct{ inner Observer }

// guardObserver wraps o so every callback is panic-safe. Idempotent.
func guardObserver(o Observer) Observer {
	if o == nil {
		o = DefaultObserver{}
	}
	if g, ok := o.(guardedObserver); ok {
		return g
	}
	return guardedObserver{inner: o}
}

func recoverObserverPanic(callback, channel, topic string) {
	if r := recover(); r != nil {
		err := fmt.Errorf("PANIC eventbus observer %s: channel=%s topic=%s: %v\n%s", callback, channel, topic, r, debug.Stack())
		log.Print(err)
		observx.ReportError(context.Background(), err)
	}
}

func (g guardedObserver) OnEmitSuccess(channel, topic string) {
	defer recoverObserverPanic("OnEmitSuccess", channel, topic)
	g.inner.OnEmitSuccess(channel, topic)
}

func (g guardedObserver) OnEmitFailure(channel, topic string, err error) {
	defer recoverObserverPanic("OnEmitFailure", channel, topic)
	g.inner.OnEmitFailure(channel, topic, err)
}

func (g guardedObserver) OnDeliverSuccess(channel, topic string) {
	defer recoverObserverPanic("OnDeliverSuccess", channel, topic)
	g.inner.OnDeliverSuccess(channel, topic)
}

func (g guardedObserver) OnDeliverFailure(channel, topic string, err error) {
	defer recoverObserverPanic("OnDeliverFailure", channel, topic)
	g.inner.OnDeliverFailure(channel, topic, err)
}

func (g guardedObserver) OnDrop(channel, topic, reason string) {
	defer recoverObserverPanic("OnDrop", channel, topic)
	g.inner.OnDrop(channel, topic, reason)
}

// OnDeliverComplete forwards only when the wrapped Observer implements
// ObserverWithLatency, so wrapping does not change which observers see
// the latency hook.
func (g guardedObserver) OnDeliverComplete(channel, topic string, duration time.Duration) {
	olat, ok := g.inner.(ObserverWithLatency)
	if !ok {
		return
	}
	defer recoverObserverPanic("OnDeliverComplete", channel, topic)
	olat.OnDeliverComplete(channel, topic, duration)
}
