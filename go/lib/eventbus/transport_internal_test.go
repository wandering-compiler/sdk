package eventbus

// White-box tests that drive the NATS + Redis adapters' non-pure
// code paths WITHOUT a live broker. The Redis client dials lazily,
// so pointing it at a refused port lets every command exercise its
// error branch; the redisSubscriber's loop + per-message plumbing
// is reached by constructing the struct directly (the broker-gated
// Subscriber() factory can't be used without a real server). NATS's
// connection is opened eagerly, so its broker methods stay e2e-only
// (see nats_e2e_test.go) — here we cover its lifecycle (Close /
// Drain) + pure helpers.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

// countObserver is a thread-safe Observer that tallies each
// callback — enough to assert which path a transport took.
type countObserver struct {
	emitOK, emitFail, delOK, delFail, drop atomic.Int32
}

func (c *countObserver) OnEmitSuccess(string, string)           { c.emitOK.Add(1) }
func (c *countObserver) OnEmitFailure(string, string, error)    { c.emitFail.Add(1) }
func (c *countObserver) OnDeliverSuccess(string, string)        { c.delOK.Add(1) }
func (c *countObserver) OnDeliverFailure(string, string, error) { c.delFail.Add(1) }
func (c *countObserver) OnDrop(string, string, string)          { c.drop.Add(1) }

// deadRedisBus builds a RedisBus whose client points at a refused
// port with retries disabled + a short dial timeout, so every
// command fails near-instantly down its error branch.
func deadRedisBus(t *testing.T, obs Observer) *RedisBus {
	t.Helper()
	bus, err := NewRedisBus(RedisBusOptions{
		DSN:               "redis://127.0.0.1:6399?max_retries=-1&dial_timeout=200ms",
		GroupPrefix:       "users-subscribers",
		ConsumerName:      "replica-1",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    20 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// Dispatch marshals OK but the XADD fails against the dead broker;
// the error returns + OnEmitFailure fires.
func TestRedisBus_DispatchError(t *testing.T) {
	obs := &countObserver{}
	bus := deadRedisBus(t, obs)
	err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("x"))
	if err == nil {
		t.Fatal("expected XADD error against dead broker")
	}
	if obs.emitFail.Load() != 1 || obs.emitOK.Load() != 0 {
		t.Errorf("emitFail=%d emitOK=%d, want 1/0", obs.emitFail.Load(), obs.emitOK.Load())
	}
}

// ensureGroup's XGROUP CREATE fails (not BUSYGROUP) against the
// dead broker → wrapped error; Subscriber() surfaces it.
func TestRedisBus_EnsureGroupError(t *testing.T) {
	bus := deadRedisBus(t, nil)
	if err := bus.ensureGroup(context.Background(), "default", "g"); err == nil {
		t.Error("expected xgroup-create error")
	}
	if _, err := bus.Subscriber(context.Background(), "default"); err == nil {
		t.Error("Subscriber should fail when the group can't be ensured")
	}
}

// Close is idempotent: the second call short-circuits on the
// closed flag.
func TestRedisBus_CloseIdempotent(t *testing.T) {
	bus := deadRedisBus(t, nil)
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("second Close should be a silent no-op: %v", err)
	}
}

// retryFor returns the bus default, or the per-channel override
// when one is configured (presence-aware merge).
func TestRedisBus_RetryFor(t *testing.T) {
	bus := deadRedisBus(t, nil)
	if md, aw := bus.retryFor("unknown"); md != 2 || aw != 20*time.Millisecond {
		t.Errorf("default retryFor = (%d,%v), want (2,20ms)", md, aw)
	}
	bus.opts.ChannelRetries = map[string]ChannelRetry{
		"hot": {MaxDeliver: 9, AckWait: 5 * time.Second},
		// zero-field override falls through to defaults:
		"warm": {},
	}
	if md, aw := bus.retryFor("hot"); md != 9 || aw != 5*time.Second {
		t.Errorf("override retryFor = (%d,%v), want (9,5s)", md, aw)
	}
	if md, aw := bus.retryFor("warm"); md != 2 || aw != 20*time.Millisecond {
		t.Errorf("zero-override should fall through to defaults, got (%d,%v)", md, aw)
	}
}

// A directly-constructed redisSubscriber's Subscribe spawns the
// consume + claim loops; Drain cancels both and the bus-shared
// WaitGroup unblocks. Against the dead broker the loops just spin
// their error branches (recover-wrapped) until cancelled.
func TestRedisSubscriber_SubscribeDrain(t *testing.T) {
	bus := deadRedisBus(t, &countObserver{})
	sub := &redisSubscriber{bus: bus, channel: "default", group: "users-subscribers-default"}

	if err := sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Let the claim ticker (20ms) fire at least once + the consume
	// loop iterate, then drain.
	time.Sleep(40 * time.Millisecond)

	if err := sub.Drain(context.Background()); err != nil {
		t.Errorf("Drain: %v", err)
	}
}

// processMessage on the success path: handler returns nil →
// OnDeliverSuccess + a best-effort XACK (which fails silently on
// the dead broker).
func TestRedisSubscriber_ProcessMessage_Success(t *testing.T) {
	obs := &countObserver{}
	bus := deadRedisBus(t, obs)
	sub := &redisSubscriber{bus: bus, channel: "default", group: "g"}

	var gotTopic, gotPayload string
	sub.processMessage(context.Background(),
		func(_ context.Context, topic string, raw []byte) error {
			gotTopic, gotPayload = topic, string(raw)
			return nil
		},
		"1-0",
		map[string]any{redisFieldTopic: "user.created", redisFieldPayload: "body"},
		0,
	)
	if gotTopic != "user.created" || gotPayload != "body" {
		t.Errorf("handler saw topic=%q payload=%q", gotTopic, gotPayload)
	}
	if obs.delOK.Load() != 1 || obs.delFail.Load() != 0 {
		t.Errorf("delOK=%d delFail=%d, want 1/0", obs.delOK.Load(), obs.delFail.Load())
	}
}

// processMessage on the failure path: handler errs → no XACK,
// OnDeliverFailure fires (the claim loop reclaims later).
func TestRedisSubscriber_ProcessMessage_Failure(t *testing.T) {
	obs := &countObserver{}
	bus := deadRedisBus(t, obs)
	sub := &redisSubscriber{bus: bus, channel: "default", group: "g"}

	sub.processMessage(context.Background(),
		func(context.Context, string, []byte) error { return context.Canceled },
		"1-0",
		map[string]any{redisFieldTopic: "user.created", redisFieldPayload: "body"},
		0,
	)
	if obs.delFail.Load() != 1 || obs.delOK.Load() != 0 {
		t.Errorf("delFail=%d delOK=%d, want 1/0", obs.delFail.Load(), obs.delOK.Load())
	}
}

// Missing stream fields degrade to empty topic/payload (the
// type-assertion fallbacks) rather than panicking.
func TestRedisSubscriber_ProcessMessage_MissingFields(t *testing.T) {
	obs := &countObserver{}
	bus := deadRedisBus(t, obs)
	sub := &redisSubscriber{bus: bus, channel: "default", group: "g"}

	var gotTopic string
	var gotLen int
	sub.processMessage(context.Background(),
		func(_ context.Context, topic string, raw []byte) error {
			gotTopic, gotLen = topic, len(raw)
			return nil
		},
		"1-0",
		map[string]any{}, // neither topic nor payload present
		0,
	)
	if gotTopic != "" || gotLen != 0 {
		t.Errorf("missing fields should yield empty topic/payload; got topic=%q len=%d", gotTopic, gotLen)
	}
}

// routeToDLQ against the dead broker: XRANGE fails → topic stays
// empty, the forward XADD is skipped, the origin XACK is best-
// effort, and OnDrop fires with the max-deliver reason.
// MED-11: against a dead broker the source XRange fails, so routeToDLQ
// CANNOT actually move the payload to the DLQ. It must therefore NOT
// ack + report OnDrop (which would silently lose a poisoned message);
// instead it surfaces OnDeliverFailure and leaves the entry in the PEL
// for the next claim tick. (Previously it reported OnDrop regardless,
// claiming a drop that never reached the DLQ.)
func TestRedisSubscriber_RouteToDLQ_BrokerDown_NoSilentDrop(t *testing.T) {
	obs := &countObserver{}
	bus := deadRedisBus(t, obs)
	sub := &redisSubscriber{bus: bus, channel: "default", group: "g"}

	sub.routeToDLQ(context.Background(), "1-0")
	if obs.drop.Load() != 0 {
		t.Errorf("OnDrop fired %d times, want 0 — the DLQ write never happened, so the message must not be reported as dropped", obs.drop.Load())
	}
	if obs.delFail.Load() == 0 {
		t.Error("OnDeliverFailure should fire when the DLQ routing can't complete (entry stays in the PEL for retry)")
	}
}

// claimPending against the dead broker: XPENDING fails → the
// function returns early (nothing to reclaim).
func TestRedisSubscriber_ClaimPending_PendingError(t *testing.T) {
	bus := deadRedisBus(t, &countObserver{})
	sub := &redisSubscriber{bus: bus, channel: "default", group: "g"}
	// Must not panic / block; just exercises the error-return path.
	sub.claimPending(context.Background(), func(context.Context, string, []byte) error { return nil }, 2, 20*time.Millisecond)
}

// ensureGroup short-circuits when the (channel, group) pair is
// already in the cache — no broker round-trip.
func TestRedisBus_EnsureGroup_Cached(t *testing.T) {
	bus := deadRedisBus(t, nil)
	bus.groups["default:g"] = true // pre-seed the cache
	if err := bus.ensureGroup(context.Background(), "default", "g"); err != nil {
		t.Errorf("cached ensureGroup should be a no-op, got %v", err)
	}
}

// Subscriber's happy path: with the group already cached,
// ensureGroup short-circuits, so Subscriber builds + tracks the
// per-channel subscriber and a later Close drains it.
func TestRedisBus_Subscriber_Success(t *testing.T) {
	bus := deadRedisBus(t, nil)
	// Pre-seed the cache for the key Subscriber will compute
	// (group = GroupPrefix + "-" + channel).
	bus.groups["default:users-subscribers-default"] = true

	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	if sub == nil {
		t.Fatal("Subscriber returned nil")
	}
	if len(bus.subscribers) != 1 {
		t.Errorf("bus should track 1 subscriber, got %d", len(bus.subscribers))
	}
	// Close drains the tracked (goroutine-free) subscriber.
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// Drain returns ctx.Err() when in-flight work outlives the drain
// deadline (the WaitGroup never reaches zero before the timeout).
func TestRedisSubscriber_Drain_Timeout(t *testing.T) {
	bus := deadRedisBus(t, nil)
	sub := &redisSubscriber{bus: bus, channel: "c", group: "g"}
	sub.wg.Add(1)
	defer sub.wg.Done() // release after the test observes the timeout
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := sub.Drain(ctx); err == nil {
		t.Error("expected drain-deadline error with in-flight work pending")
	}
}

// --- NATS lifecycle + helpers (no broker) -----------------------------

// Close on a freshly-constructed (never-connected) bus drains its
// empty subscriber set + skips the nil connection; second call is
// a no-op via the closed flag.
func TestNatsBus_Close_NoConn(t *testing.T) {
	b := &NatsBus{streams: map[string]bool{}}
	if err := b.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Errorf("second Close should no-op: %v", err)
	}
}

// natsSubscriber.Drain with handles whose consume-context +
// advisory subscription are nil walks the cleanup loop (both
// nil-guards skipped) and unblocks on the empty WaitGroup.
func TestNatsSubscriber_Drain_NilHandles(t *testing.T) {
	sub := &natsSubscriber{
		bus:     &NatsBus{streams: map[string]bool{}},
		channel: "default",
		handles: []*natsConsumeHandle{{}}, // cctx + advisorySub both nil
	}
	if err := sub.Drain(context.Background()); err != nil {
		t.Errorf("Drain: %v", err)
	}
	if sub.handles != nil {
		t.Error("Drain should have cleared the handles slice")
	}
}

// natsSubscriber.Drain returns ctx.Err() when in-flight handlers
// outlive the drain deadline.
func TestNatsSubscriber_Drain_Timeout(t *testing.T) {
	b := &NatsBus{streams: map[string]bool{}}
	sub := &natsSubscriber{bus: b, channel: "default"}
	sub.wg.Add(1)
	defer sub.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := sub.Drain(ctx); err == nil {
		t.Error("expected drain-deadline error with in-flight work pending")
	}
}

// natsMaxDeliverAdvisorySubject composes the JetStream advisory
// subject the server publishes past-MaxDeliver drops on.
func TestNatsMaxDeliverAdvisorySubject(t *testing.T) {
	got := natsMaxDeliverAdvisorySubject("orders", "users-subscribers-orders")
	want := "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.orders.users-subscribers-orders"
	if got != want {
		t.Errorf("advisory subject = %q, want %q", got, want)
	}
}

// --- loop-tick recovery + Nop observer --------------------------------

// recoverLoopTick swallows a panic in one loop iteration so the
// long-lived consume/claim goroutine survives to the next tick.
func TestRecoverLoopTick_RecoversPanic(t *testing.T) {
	survived := false
	func() {
		defer func() { survived = true }()
		defer recoverLoopTick(context.Background(), "test loop")
		panic("iteration blew up")
	}()
	if !survived {
		t.Fatal("recoverLoopTick let the panic escape the iteration")
	}
}

// NopObserver's callbacks are silent no-ops — the default wired
// when a caller leaves Options.Observer nil.
func TestNopObserver_Internal(t *testing.T) {
	var nop NopObserver
	nop.OnEmitSuccess("c", "t")
	nop.OnEmitFailure("c", "t", context.Canceled)
	nop.OnDeliverSuccess("c", "t")
	nop.OnDeliverFailure("c", "t", context.Canceled)
	nop.OnDrop("c", "t", DropReasonBufferFull)
}
