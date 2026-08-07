package eventbus_test

// Black-box tests that drive the Redis Streams adapter against
// an IN-PROCESS miniredis server — no Docker. miniredis
// implements the Streams surface the adapter relies on (XADD,
// XGROUP, XREADGROUP with BLOCK, XACK, XPENDING with IDLE,
// XCLAIM, XRANGE, XLEN), so the consume loop, the claim/redeliver
// loop, the DLQ-routing path and the drain/close lifecycle all
// exercise their real broker round-trips here.
//
// These reproduce the coverage of the //go:build e2e
// redis_e2e_test.go suite (which uses real containers) so the
// default `go test` run exercises the same behaviours.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// startEmbeddedRedis runs an in-process miniredis server (torn
// down via t.Cleanup) and returns the `redis://` DSN go-redis
// parses.
func startEmbeddedRedis(t *testing.T) string {
	t.Helper()
	mr := miniredis.RunT(t)
	return "redis://" + mr.Addr()
}

// rawRedisClient opens a bare go-redis client against the same
// DSN for direct stream inspection (XLEN of the DLQ key) — the
// bus surface intentionally hides its underlying client.
func rawRedisClient(t *testing.T, dsn string) *goredis.Client {
	t.Helper()
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis dsn %q: %v", dsn, err)
	}
	c := goredis.NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// INVARIANT: a dispatched envelope round-trips through the Redis
// Streams broker — the consumer-group reader hands the handler
// the dispatch-time topic + non-empty payload, and the bus
// instruments emit + deliver success via the Observer.
func TestRedisBus_RoundTrip_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "roundtrip-subscribers",
		Observer:    obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}

	type delivery struct {
		topic string
		raw   []byte
	}
	received := make(chan delivery, 1)
	if err := sub.Subscribe(context.Background(), "**",
		func(_ context.Context, topic string, raw []byte) error {
			received <- delivery{topic, raw}
			return nil
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("hello-redis")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case got := <-received:
		if got.topic != "user.created" {
			t.Errorf("topic = %q, want user.created", got.topic)
		}
		if len(got.raw) == 0 {
			t.Error("payload empty")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler never invoked in 10s — Redis round-trip broken")
	}

	waitFor(t, 3*time.Second, "emit+deliver success", func() bool {
		es, _, ds, _, _ := obs.snapshots()
		return es >= 1 && ds >= 1
	})
}

// INVARIANT: a handler error leaves the entry unacked in the
// PEL; after AckWait the claim loop XCLAIMs + replays it, and the
// second attempt succeeds + XACKs. Observer records >=1 deliver
// failure and >=1 deliver success.
func TestRedisBus_HandlerErrorClaimRedelivers_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               startEmbeddedRedis(t),
		GroupPrefix:       "retry-subscribers",
		DefaultMaxDeliver: 5,
		DefaultAckWait:    200 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	done := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(_ context.Context, _ string, _ []byte) error {
			if attempts.Add(1) == 1 {
				return errors.New("transient handler error")
			}
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("claim-loop redelivery never succeeded; attempts = %d", attempts.Load())
	}

	if got := attempts.Load(); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (initial + claim redelivery)", got)
	}
	waitFor(t, 3*time.Second, "deliver failure+success", func() bool {
		_, _, ds, df, _ := obs.snapshots()
		return df >= 1 && ds >= 1
	})
}

// TestRedisBus_DrainOneOfTwoSubscribers_Embedded — Q36-bus-2. Draining
// ONE subscriber must not block on a SIBLING subscriber's still-running
// loops. Pre-fix the consume+claim loops Add to a BUS-shared WaitGroup,
// so subA.Drain (which Waits on that shared wg) can never reach zero
// while subB's two loop tokens are held → it blocks for the whole ctx
// timeout (or deadlocks under a non-cancellable ctx, as Close uses).
func TestRedisBus_DrainOneOfTwoSubscribers_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "drain-one-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	noop := func(_ context.Context, _ string, _ []byte) error { return nil }
	subA, err := bus.Subscriber(context.Background(), "chanA")
	if err != nil {
		t.Fatalf("Subscriber A: %v", err)
	}
	if err := subA.Subscribe(context.Background(), "**", noop); err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	subB, err := bus.Subscriber(context.Background(), "chanB")
	if err != nil {
		t.Fatalf("Subscriber B: %v", err)
	}
	if err := subB.Subscribe(context.Background(), "**", noop); err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}

	// Drain ONLY subA, with a finite timeout, while subB stays subscribed.
	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := subA.Drain(dctx); err != nil {
		t.Fatalf("Drain(subA) blocked %v on a still-active sibling subscriber: %v", time.Since(start), err)
	}
}

// INVARIANT: an always-failing handler drives the PEL delivery
// counter past MaxDeliver; the claim loop XADDs the original
// entry to `<channel>.dlq`, XACKs the origin, and fires
// Observer.OnDrop with the canonical max-deliver reason.
func TestRedisBus_DLQRouting_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "dlq-subscribers",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    200 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			attempts.Add(1)
			return errors.New("always fails — drives DLQ path")
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed"))

	waitFor(t, 8*time.Second, "OnDrop from DLQ routing", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})

	// The original payload must have been forwarded to the DLQ
	// stream before the origin entry was XACK'd.
	raw := rawRedisClient(t, dsn)
	xlen, err := raw.XLen(context.Background(), "default.dlq").Result()
	if err != nil {
		t.Fatalf("XLEN default.dlq: %v", err)
	}
	if xlen < 1 {
		t.Errorf("DLQ stream `default.dlq` has %d entries, want >= 1", xlen)
	}
}

// INVARIANT: a per-channel ChannelRetries override drives the
// claim loop's DLQ cutoff below the (high) bus default — an
// always-failing handler routes to DLQ after the override's
// count, capping handler invocations well under the default.
func TestRedisBus_ChannelRetriesOverride_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "retryoverride-subscribers",
		DefaultMaxDeliver: 20,
		DefaultAckWait:    200 * time.Millisecond,
		ChannelRetries: map[string]eventbus.ChannelRetry{
			"default": {MaxDeliver: 2},
		},
		Observer: obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			attempts.Add(1)
			return errors.New("always fails — drives override DLQ cutoff")
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed"))

	waitFor(t, 5*time.Second, "OnDrop within override window", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})

	// Stop the claim loop before sampling attempts so a late
	// XCLAIM can't race the assertion.
	_ = sub.Drain(timeout(t, 3*time.Second))

	if got := attempts.Load(); got > 5 {
		t.Errorf("attempts = %d, want <= 5 (override.MaxDeliver=2; default would allow 20)", got)
	}

	raw := rawRedisClient(t, dsn)
	xlen, err := raw.XLen(context.Background(), "default.dlq").Result()
	if err != nil {
		t.Fatalf("XLEN default.dlq: %v", err)
	}
	if xlen < 1 {
		t.Errorf("DLQ stream `default.dlq` has %d entries, want >= 1", xlen)
	}
}

// INVARIANT: Drain cancels the consume + claim loops; dispatches
// issued after Drain returns never reach the handler.
func TestRedisBus_DrainStopsCleanly_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "drain-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var calls atomic.Int32
	gotOne := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			calls.Add(1)
			select {
			case gotOne <- struct{}{}:
			default:
			}
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	select {
	case <-gotOne:
	case <-time.After(10 * time.Second):
		t.Fatal("first delivery never arrived")
	}

	if err := sub.Drain(timeout(t, 3*time.Second)); err != nil {
		t.Errorf("Drain: %v", err)
	}

	preCalls := calls.Load()
	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("y"))
	time.Sleep(600 * time.Millisecond)
	if got := calls.Load(); got != preCalls {
		t.Errorf("handler fired %d more times after Drain (was %d, now %d)", got-preCalls, preCalls, got)
	}
}

// INVARIANT: concurrent publishers + a consumer-group subscriber
// on one stream stay race-clean (run under -race) and every
// published envelope is delivered.
func TestRedisBus_ConcurrentPublishSubscribe_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "concurrent-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	const total = 50
	var delivered atomic.Int32
	allSeen := make(chan struct{})
	var once sync.Once
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			if delivered.Add(1) >= total {
				once.Do(func() { close(allSeen) })
			}
			return nil
		})

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = bus.Dispatch(context.Background(), "default", "evt", wrapperspb.Int64(int64(n)))
		}(i)
	}
	wg.Wait()

	select {
	case <-allSeen:
	case <-time.After(15 * time.Second):
		t.Fatalf("only %d/%d envelopes delivered", delivered.Load(), total)
	}
}

// INVARIANT: Close drains every tracked subscriber + closes the
// go-redis client; the second call short-circuits on the closed
// flag (idempotent) against a live broker.
func TestRedisBus_CloseIdempotent_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "close-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	sub, _ := bus.Subscriber(context.Background(), "default")
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { return nil })

	if err := bus.Close(timeout(t, 5*time.Second)); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := bus.Close(timeout(t, 5*time.Second)); err != nil {
		t.Errorf("second Close should no-op: %v", err)
	}
}

// INVARIANT (R-bus-1 + R-bus-6): a message whose handler is still
// in-flight when its PEL delivery counter is already at MaxDeliver must
// NOT be routed to the DLQ + XACK'd out from under the running handler.
// The in-flight skip in claimPending runs BEFORE the max-deliver/DLQ
// branch, so the in-flight final attempt is left alone; on success it
// XACKs itself (no DLQ, no spurious OnDrop) and is processed exactly
// once.
//
// Repro relies on the two-goroutine layout: the CONSUME loop delivers
// the message (deliveryCount=1) and its handler blocks past AckWait,
// while the independent CLAIM loop ticks concurrently and sees the same
// entry. With MaxDeliver=1 the very first (in-flight) delivery is
// already AT the cap, so the buggy ordering (DLQ branch first) would
// DLQ + ack it mid-flight. (Note: a claim-loop reclaim can't reproduce
// this — claimPending invokes the handler synchronously, so a blocking
// retry stalls the claim loop itself; the race needs the separate
// consume-loop delivery.)
func TestRedisBus_InFlightAtCapNotDLQd_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "inflight-subscribers",
		DefaultMaxDeliver: 1, // first (in-flight) delivery is already the final attempt
		DefaultAckWait:    150 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	done := make(chan struct{})
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			attempts.Add(1)
			// Block well past AckWait so the claim loop ticks several times
			// while this consume-loop handler is still in-flight at the cap.
			time.Sleep(1 * time.Second)
			close(done)
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("survivor"))

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("in-flight handler never completed; attempts=%d", attempts.Load())
	}

	// Give any spurious post-completion DLQ tick a chance to (wrongly) fire.
	time.Sleep(300 * time.Millisecond)

	if _, _, _, _, drop := obs.snapshots(); drop != 0 {
		t.Errorf("OnDrop fired %d times — an in-flight at-cap message was spuriously DLQ'd while still processing", drop)
	}
	raw := rawRedisClient(t, dsn)
	// XLEN of a missing key is 0; ignore a possible miss error.
	xlen, _ := raw.XLen(context.Background(), "default.dlq").Result()
	if xlen != 0 {
		t.Errorf("DLQ stream `default.dlq` has %d entries; an in-flight message was DLQ'd while still processing", xlen)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts=%d, want exactly 1 (one in-flight delivery, XACK'd on success — no DLQ-triggered redelivery)", got)
	}
	if _, _, ds, _, _ := obs.snapshots(); ds < 1 {
		t.Error("expected >=1 deliver success (the in-flight handler succeeded + XACK'd)")
	}
}

// INVARIANT (R-bus-4): Dispatch detaches from the caller's ctx, so an
// emit issued on an already-cancelled RPC ctx is still published +
// delivered — fire-and-forget emits must not be lost just because the
// originating request finished.
func TestRedisBus_DispatchDetachesCanceledCtx_Embedded(t *testing.T) {
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         startEmbeddedRedis(t),
		GroupPrefix: "detach-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	received := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			select {
			case received <- struct{}{}:
			default:
			}
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE dispatch
	if err := bus.Dispatch(ctx, "default", "t", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch on a cancelled ctx should still publish (detached): %v", err)
	}

	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("envelope dispatched on a cancelled ctx never delivered — detach broken")
	}
}

// INVARIANT (R-bus-7): if the consumer group disappears under the bus
// — a Redis-as-cache FLUSHALL or a restart of a Redis without
// persistence drops the stream + its groups — the consume loop must
// recreate the group and resume delivery. The bug: XREADGROUP then
// fails with NOGROUP forever, which the loop misclassifies as a
// "transient hiccup" and retries IMMEDIATELY (a 100% CPU tight spin),
// while the cached `groups` flag masks the loss so the group is never
// recreated and delivery stalls permanently.
func TestRedisBus_RecoversFromLostGroup_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         dsn,
		GroupPrefix: "lostgroup-subscribers",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	received := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			select {
			case received <- struct{}{}:
			default:
			}
			return nil
		})

	// Confirm the subscriber is live with one warm-up round-trip.
	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("warmup"))
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("warm-up delivery never arrived")
	}

	// Simulate the broker losing the consumer group (cache flush /
	// restart without persistence). The bus's groups-cache still
	// believes the group exists.
	raw := rawRedisClient(t, dsn)
	if err := raw.XGroupDestroy(context.Background(), "default", "lostgroup-subscribers-default").Err(); err != nil {
		t.Fatalf("destroy group: %v", err)
	}

	// Keep dispatching; a healthy bus recreates the group and resumes
	// delivery. A buggy one spins on NOGROUP and never delivers again.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(150 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("after-loss"))
			}
		}
	}()

	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("delivery never resumed after the consumer group was lost — " +
			"consume loop spins on NOGROUP without recreating the group")
	}
}
