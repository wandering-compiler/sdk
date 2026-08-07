package eventbus_test

// Lifecycle gates on the two production adapters: what a bus and a subscriber
// must REFUSE once they are being torn down, and whose in-flight bookkeeping a
// message belongs to.
//
// The shipped shutdown path is `bus.Close` → `Drain` per registered
// subscriber. Both halves were check-then-act on shared state with no gate:
//
//   - `Subscriber()` never consulted `closed`, and `Close` never clears the
//     stream/group caches — so on a channel used before Close, a later
//     `Subscriber()` did zero broker I/O and handed back a live handle with a
//     nil error, registered after Close's snapshot and therefore drained by
//     nobody.
//   - `Subscribe()` never consulted `draining`, and its handle/loops are
//     registered AFTER the broker consumer is already live — so a Subscribe
//     that overlapped Drain left a consumer nothing would ever stop. On NATS
//     its deliveries are Nak'd to the delivery cap and the advisory that
//     should dead-letter them is itself gated on `draining`, so the poison
//     message reaches neither the handler nor the DLQ nor OnDrop. On Redis
//     the loops keep XREADGROUP-ing, then spin on the error backoff against a
//     closed client, with a cancel func no one holds.
//
// All three are written as invariants (an error return, an observable absence
// of delivery), not as race-detector runs.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// INVARIANT (B-F6): after Close returns, the bus hands out no more
// subscribers — on a WARM channel too, where the cached stream/group makes
// the whole call local. A handle minted after Close's snapshot is registered
// with nobody to drain it and bound to a connection that is gone.
func TestNatsBus_SubscriberAfterCloseRefused_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "close-gate",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	ctx := timeout(t, 10*time.Second)

	// Warm the stream cache so the post-Close call needs no broker round-trip.
	if _, err := bus.Subscriber(ctx, "default"); err != nil {
		t.Fatalf("pre-close Subscriber: %v", err)
	}
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if sub, err := bus.Subscriber(ctx, "default"); err == nil {
		t.Fatalf("Subscriber after Close returned a live handle (%T) with a nil error — "+
			"it is registered after Close's snapshot, so nothing will ever drain it", sub)
	}
	// A cold channel must be refused for the same reason, not merely because
	// the closed connection happens to fail the broker call.
	if _, err := bus.Subscriber(ctx, "never-used"); err == nil {
		t.Fatal("Subscriber after Close returned a handle for a cold channel")
	}
}

// INVARIANT (B-F6), Redis half: identical rule, and here the warm path is
// entirely local — ensureGroup short-circuits on the bus's groups cache,
// which Close never clears.
func TestRedisBus_SubscriberAfterCloseRefused_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "close-gate",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	ctx := timeout(t, 10*time.Second)

	if _, err := bus.Subscriber(ctx, "default"); err != nil {
		t.Fatalf("pre-close Subscriber: %v", err)
	}
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if sub, err := bus.Subscriber(ctx, "default"); err == nil {
		t.Fatalf("Subscriber after Close returned a live handle (%T) with a nil error — "+
			"registered after Close's snapshot, never drained, bound to a closed client", sub)
	}
	if _, err := bus.Subscriber(ctx, "never-used"); err == nil {
		t.Fatal("Subscriber after Close returned a handle for a cold channel")
	}
}

// INVARIANT (B-F3): a Subscribe that begins after Drain must not leave a
// consumer running. Drain's snapshot is taken before the handle is appended,
// so a late Subscribe would hand back success while its consumer belongs to
// nobody — and on NATS its deliveries then die silently: the consume callback
// Naks them to the cap and the MAX_DELIVERIES advisory handler is gated on the
// same draining flag, so nothing reaches the DLQ or OnDrop.
func TestNatsSubscriber_SubscribeAfterDrainRefused_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "drain-gate",
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	ctx := timeout(t, 20*time.Second)
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, err := bus.Subscriber(ctx, "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int32
	handler := func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}
	if err := sub.Subscribe(ctx, "**", handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Drain(timeout(t, 10*time.Second)); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if err := sub.Subscribe(ctx, "**", handler); err == nil {
		t.Error("Subscribe succeeded on a drained subscriber — its consumer is " +
			"registered after Drain's snapshot, so nothing will ever stop it")
	}

	// Whatever the call returned, no consumer of this subscriber may still be
	// taking messages off the channel.
	if err := bus.Dispatch(ctx, "default", "t", wrapperspb.String("after-drain")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := delivered.Load(); got != 0 {
		t.Errorf("a drained subscriber consumed %d message(s) — leaked live consumer", got)
	}
}

// INVARIANT (B-F4), the Redis half of the same rule. The Redis subscriber had
// no draining gate at all: a late Subscribe appends its cancel func to a slice
// Drain has already emptied, so the two loops it spawns can never be
// cancelled — after Close they spin on the 200ms error backoff for the life of
// the process. (The same gate also makes the wg.Add atomic with the flag
// check, closing the documented Add-concurrent-with-Wait-at-zero misuse.)
func TestRedisSubscriber_SubscribeAfterDrainRefused_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         dsn,
		GroupPrefix: "drain-gate",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	ctx := timeout(t, 20*time.Second)
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, err := bus.Subscriber(ctx, "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int32
	handler := func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}
	if err := sub.Subscribe(ctx, "**", handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Drain(timeout(t, 10*time.Second)); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if err := sub.Subscribe(ctx, "**", handler); err == nil {
		t.Error("Subscribe succeeded on a drained subscriber — its consume + claim " +
			"loops hold a cancel func Drain will never call")
	}

	if err := bus.Dispatch(ctx, "default", "t", wrapperspb.String("after-drain")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := delivered.Load(); got != 0 {
		t.Errorf("a drained subscriber consumed %d message(s) — uncancellable loops", got)
	}
}

// INVARIANT (B-F9): the in-flight guard belongs to the CONSUMER IDENTITY
// (stream + group + consumer name), not to the subscriber object. Two
// subscribers minted from one bus for one channel share a PEL identity, so the
// second one's claim loop must not reclaim, dead-letter and XACK a message the
// first one's handler is still running — the incoherent drop+success the guard
// exists to prevent, through its blind spot.
func TestRedisBus_InFlightGuardIsPerConsumerIdentity_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "shared-identity",
		DefaultMaxDeliver: 1, // the in-flight delivery is already at the cap
		DefaultAckWait:    150 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	ctx := timeout(t, 30*time.Second)
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	first, err := bus.Subscriber(ctx, "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	entered := make(chan struct{})
	finished := make(chan struct{})
	var attempts atomic.Int32
	if err := first.Subscribe(ctx, "**", func(context.Context, string, []byte) error {
		if attempts.Add(1) == 1 {
			close(entered)
			// Block well past AckWait so the sibling's claim loop ticks
			// several times while this handler is still in flight.
			time.Sleep(1200 * time.Millisecond)
			close(finished)
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Dispatch(ctx, "default", "t", wrapperspb.String("survivor")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never received the message")
	}

	// A SECOND subscriber on the same channel: same group, same consumer name,
	// therefore the same PEL entries — including the one in flight above.
	second, err := bus.Subscriber(ctx, "default")
	if err != nil {
		t.Fatalf("second Subscriber: %v", err)
	}
	if err := second.Subscribe(ctx, "**", func(context.Context, string, []byte) error {
		return nil
	}); err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight handler never completed")
	}
	time.Sleep(300 * time.Millisecond)

	if _, _, _, _, drop := obs.snapshots(); drop != 0 {
		t.Errorf("OnDrop fired %d time(s) — a sibling subscriber dead-lettered a "+
			"message whose handler was still running", drop)
	}
	raw := rawRedisClient(t, dsn)
	if xlen, _ := raw.XLen(context.Background(), "default.dlq").Result(); xlen != 0 {
		t.Errorf("DLQ `default.dlq` holds %d entr(ies) — the in-flight message was "+
			"DLQ'd + XACK'd out from under its running handler", xlen)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("handler ran %d times, want exactly 1 — the message was reclaimed "+
			"while in flight", got)
	}
}
