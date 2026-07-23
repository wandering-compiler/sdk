package eventbus

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// recoverHandler wraps a HandlerFunc so a panic in event-handler
// business logic (a nil-deref on a malformed envelope, a user bug)
// is recovered into a delivery error instead of crashing the
// consumer goroutine. That goroutine is detached from any request
// path, so its panic is NOT caught by the per-handler gRPC recover
// or the REST middleware — uncaught, it takes down the whole
// process. Here the panic value + stack are routed through observx
// (Sentry + OTel; a panic is always "not noise" per quality.md
// §Sentry), and the returned error flows into the transport's normal
// delivery-failure path (NAK / retry / DLQ), so a poisoned message
// is retried or dead-lettered — never fatal. Every transport's
// Subscribe wraps its handler with this before feeding deliveries.
func recoverHandler(h HandlerFunc) HandlerFunc {
	return func(ctx context.Context, topic string, envelope []byte) (err error) {
		defer func() {
			if r := recover(); r != nil {
				observx.ReportError(ctx, fmt.Errorf("PANIC event handler %q: %v\n%s", topic, r, debug.Stack()))
				err = fmt.Errorf("event handler panic on %q: %v", topic, r)
			}
		}()
		return h(ctx, topic, envelope)
	}
}

// recoverLoopTick recovers a panic in ONE iteration of a long-lived
// consume/claim loop so the panic neither crashes the process nor
// silently kills the detached consumer goroutine — the loop continues to
// the next iteration. The handler body is already covered by
// recoverHandler; this is the belt-and-suspenders net around the loop
// plumbing itself (transport client calls, dispatch). Use as the first
// statement of the per-iteration work: `defer recoverLoopTick(ctx, label)`.
func recoverLoopTick(ctx context.Context, label string) {
	if r := recover(); r != nil {
		observx.ReportError(ctx, fmt.Errorf("PANIC %s: %v\n%s", label, r, debug.Stack()))
	}
}

// HandlerFunc is the callback shape every transport adapter
// invokes per delivered message. Receives the original
// envelope topic (so server-side filtering in the generated
// dispatch can match against per-subscription globs) + the
// raw serialised envelope bytes (decoded by the generated
// dispatch via proto.Unmarshal into the per-domain envelope
// type).
type HandlerFunc func(ctx context.Context, topic string, envelope []byte) error

// Subscriber attaches to one channel's transport and feeds
// envelope deliveries to a HandlerFunc. Phase 10 ships the
// concrete implementations (NATS JetStream, Redis Streams,
// in-process MEMORY); Phase 9 only depends on the contract.
//
// The Subscribe / Drain split mirrors the typical broker
// client shape — Subscribe registers + starts feeding,
// Drain waits for in-flight deliveries to complete on
// SIGTERM (bounded by the channel's drain_timeout_seconds
// from the parser).
//
// The generated subscriber dispatcher always Subscribes with
// the catch-all topic filter `**` and re-checks per-
// subscription globs in the dispatch switch — that keeps the
// per-channel Subscribe count at one even when multiple
// subscriptions on the same channel use different filters.
// Transport adapters may push the catch-all down (NATS subject
// glob, Redis stream key) without affecting correctness.
type Subscriber interface {
	Subscribe(ctx context.Context, topicFilter string, handler HandlerFunc) error
	Drain(ctx context.Context) error
}

// SubscriberFactory hands out per-channel Subscribers. The
// generated dispatcher's RunSubscribers calls
// `factory.Subscriber(ctx, "<channel>")` for every channel
// the surface consumes. Per-channel DSN comes from the
// runtime env (`W17_QUEUE_<NAME>`) — the factory hides the
// env lookup so generated code doesn't read env directly.
//
// Phase 10's SubscriberFactory implementation registers
// transport adapters keyed by Channel.Transport from the
// parser's ChannelRegistry.
type SubscriberFactory interface {
	Subscriber(ctx context.Context, channel string) (Subscriber, error)
}

// MatchGlob reports whether `topic` matches the glob `filter`.
// Both are dot-separated segments; each filter segment is
// either:
//
//	`**`     — matches zero or more whole topic segments
//	`*`      — matches exactly one whole topic segment
//	literal  — matches one segment exactly
//
// Examples:
//
//	MatchGlob("user.*",   "user.created")        -> true
//	MatchGlob("user.*",   "user.created.email")  -> false
//	MatchGlob("user.**",  "user.created.email")  -> true
//	MatchGlob("**",       "anything.here")       -> true
//	MatchGlob("user.created", "user.created")    -> true
//
// Empty filter matches empty topic only; empty topic matches
// `**` or empty filter.
//
// The matcher is recursive over filter segments; `**` does
// backtracking over the topic-segment split-points.
func MatchGlob(filter, topic string) bool {
	if filter == "" {
		return topic == ""
	}
	// Allocation-free fast paths for the two dominant shapes, so the hot
	// emit loop doesn't split both strings on every handler match:
	//   - "**" is the catch-all the generated subscriber always registers
	//     with (see Subscriber godoc) → matches any topic.
	//   - a filter with no wildcard byte is a pure literal: matchSegments
	//     then reduces to elementwise segment equality, which (splitting on
	//     "." being injective) is exactly string equality.
	// Genuine globs still fall through to the recursive split-based matcher.
	if filter == "**" {
		return true
	}
	if !strings.ContainsRune(filter, '*') {
		return filter == topic
	}
	return matchSegments(strings.Split(filter, "."), strings.Split(topic, "."))
}

// matchSegments is MatchGlob's recursive worker. `**` at the
// end short-circuits true; non-terminal `**` tries every
// remaining topic-segment alignment until one matches the
// suffix.
func matchSegments(filter, topic []string) bool {
	if len(filter) == 0 {
		return len(topic) == 0
	}
	if filter[0] == "**" {
		if len(filter) == 1 {
			return true
		}
		for i := 0; i <= len(topic); i++ {
			if matchSegments(filter[1:], topic[i:]) {
				return true
			}
		}
		return false
	}
	if len(topic) == 0 {
		return false
	}
	if filter[0] == "*" || filter[0] == topic[0] {
		return matchSegments(filter[1:], topic[1:])
	}
	return false
}
