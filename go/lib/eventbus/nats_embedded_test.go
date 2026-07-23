package eventbus_test

// Black-box tests that drive the NATS JetStream adapter against
// an IN-PROCESS embedded nats-server (JetStream enabled, store
// dir under t.TempDir()) — no Docker. These reach the broker-
// gated code the white-box transport_internal_test.go cannot:
// Dispatch / ensureStream / Subscriber / Subscribe and their
// ack / Nak / redelivery / MaxDeliver-advisory / drain paths.
//
// They reproduce the coverage of the //go:build e2e
// nats_e2e_test.go suite (which uses real containers) so the
// default `go test` run exercises the same behaviours.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// startEmbeddedNATS spins up an in-process JetStream-enabled
// nats-server on an ephemeral port, tears it down via
// t.Cleanup, and returns its client URL. Each test gets its
// own server + store dir so durable-consumer / stream state
// never leaks across tests.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready for connections in 5s")
	}
	return srv.ClientURL()
}

// waitFor polls cond until it returns true or the budget
// expires, failing the test on timeout. Keeps the assertion
// loops in the NATS/Redis embedded suites compact.
func waitFor(t *testing.T, budget time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never met within %v: %s", budget, msg)
}

// INVARIANT: a dispatched envelope round-trips through a real
// JetStream broker — the subscribed handler sees the dispatch-
// time topic + non-empty payload, and the bus instruments emit
// + deliver success through the Observer.
func TestNatsBus_RoundTrip_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "roundtrip-subscribers",
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
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

	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("hello-nats")); err != nil {
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
		t.Fatal("handler never invoked in 10s — NATS round-trip broken")
	}

	waitFor(t, 3*time.Second, "emit+deliver success", func() bool {
		es, _, ds, _, _ := obs.snapshots()
		return es >= 1 && ds >= 1
	})
}

// INVARIANT: a handler that errors once is Nak'd and JetStream
// redelivers; the retry succeeds. The Observer records at least
// one deliver failure and one deliver success.
func TestNatsBus_HandlerErrorRedelivers_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "retry-subscribers",
		DefaultMaxDeliver: 5,
		DefaultAckWait:    500 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
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
	case <-time.After(15 * time.Second):
		t.Fatalf("redelivery never succeeded; attempts = %d", attempts.Load())
	}

	if got := attempts.Load(); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (initial + retry)", got)
	}
	waitFor(t, 3*time.Second, "deliver failure+success", func() bool {
		_, _, ds, df, _ := obs.snapshots()
		return df >= 1 && ds >= 1
	})
}

// INVARIANT: an always-failing handler exhausts MaxDeliver; the
// server publishes a MAX_DELIVERIES advisory, and the bus's
// per-handle advisory subscription surfaces it as Observer.OnDrop
// with the canonical max-deliver reason. Handler runs exactly
// MaxDeliver times.
func TestNatsBus_MaxDeliverAdvisoryDrop_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "maxdeliveradv-subscribers",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    300 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(_ context.Context, _ string, _ []byte) error {
			attempts.Add(1)
			return errors.New("always fails — drives past MaxDeliver")
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed"))

	waitFor(t, 6*time.Second, "OnDrop from MAX_DELIVERIES advisory", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want exactly 2 (MaxDeliver=2)", got)
	}
	if _, _, _, df, _ := obs.snapshots(); df < 2 {
		t.Errorf("deliver failures = %d, want >= 2 (one per Nak)", df)
	}
}

// INVARIANT: each JetStream stream is isolated — an emit on
// channel `audit` reaches only the `audit` subscriber and never
// the `default` one.
func TestNatsBus_ChannelIsolation_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "isolation-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	defaultSub, _ := bus.Subscriber(context.Background(), "default")
	auditSub, _ := bus.Subscriber(context.Background(), "audit")

	var defaultCalls, auditCalls atomic.Int32
	_ = defaultSub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { defaultCalls.Add(1); return nil })
	_ = auditSub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { auditCalls.Add(1); return nil })

	_ = bus.Dispatch(context.Background(), "audit", "billing.audited", wrapperspb.String("x"))

	waitFor(t, 10*time.Second, "audit handler fires", func() bool { return auditCalls.Load() >= 1 })
	// Give a stray cross-stream delivery a chance to (wrongly) land.
	time.Sleep(300 * time.Millisecond)
	if got := auditCalls.Load(); got != 1 {
		t.Errorf("audit handler fired %d times, want 1", got)
	}
	if got := defaultCalls.Load(); got != 0 {
		t.Errorf("default handler fired %d times on audit emit; want 0", got)
	}
}

// INVARIANT: a per-channel ChannelRetries override caps
// MaxDeliver below the (high) bus default — an always-failing
// handler stops after the override's count, not the default's.
func TestNatsBus_ChannelRetriesOverride_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "retryoverride-subscribers",
		DefaultMaxDeliver: 20,
		DefaultAckWait:    300 * time.Millisecond,
		ChannelRetries: map[string]eventbus.ChannelRetry{
			"default": {MaxDeliver: 2},
		},
		Observer: obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			attempts.Add(1)
			return errors.New("always fails — drives the MaxDeliver cap")
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed"))

	// With override.MaxDeliver=2 + AckWait=300ms, delivery
	// stops after ~2 attempts (<1s). DefaultMaxDeliver=20 would
	// keep ticking for ~6s. Wait well past the override window.
	time.Sleep(3 * time.Second)

	got := attempts.Load()
	if got > 3 {
		t.Errorf("attempts = %d, want <= 3 (override.MaxDeliver=2; default would allow 20)", got)
	}
	if got < 2 {
		t.Errorf("attempts = %d, want >= 2 (initial + one redelivery)", got)
	}
}

// INVARIANT: Drain stops the consume loop — dispatches issued
// after Drain returns never reach the handler.
func TestNatsBus_DrainStopsCleanly_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "drain-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
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

	if err := sub.Drain(timeout(t, 5*time.Second)); err != nil {
		t.Errorf("Drain: %v", err)
	}

	preCalls := calls.Load()
	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("y"))
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != preCalls {
		t.Errorf("handler fired %d more times after Drain (was %d, now %d)", got-preCalls, preCalls, got)
	}
}

// INVARIANT: concurrent publishers + a subscriber on one channel
// stay race-clean (run under -race) and every published envelope
// is delivered exactly once.
func TestNatsBus_ConcurrentPublishSubscribe_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "concurrent-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")

	const total = 50
	var delivered atomic.Int32
	allSeen := make(chan struct{})
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			if delivered.Add(1) == total {
				close(allSeen)
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

// INVARIANT: Close drains the live connection and is idempotent
// against a connected broker (second call is a silent no-op).
func TestNatsBus_CloseIdempotent_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "close-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
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

// INVARIANT (R-bus-3): a message that exhausts MaxDeliver is forwarded
// to the channel's dead-letter stream (`<channel>_dlq`, subject
// `_dlq.<channel>.<topic>`) — payload + topic preserved — and only
// THEN reported via Observer.OnDrop. Parity with the Redis adapter's
// `<channel>.dlq`; poison messages are never silently dropped.
func TestNatsBus_DLQRouting_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               url,
		DurablePrefix:     "dlq-subscribers",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    300 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			return errors.New("always fails — drives past MaxDeliver into the DLQ")
		})

	_ = bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("doomed"))

	waitFor(t, 10*time.Second, "OnDrop after DLQ routing", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})

	// OnDrop fires only AFTER the DLQ publish, so the message must now be
	// in `default_dlq`. Open a raw JetStream view to inspect it.
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx := context.Background()
	dlq, err := js.Stream(ctx, "default_dlq")
	if err != nil {
		t.Fatalf("dlq stream default_dlq: %v", err)
	}
	info, err := dlq.Info(ctx)
	if err != nil {
		t.Fatalf("dlq info: %v", err)
	}
	if info.State.Msgs < 1 {
		t.Fatalf("DLQ stream default_dlq has %d msgs, want >= 1", info.State.Msgs)
	}

	raw, err := dlq.GetMsg(ctx, info.State.FirstSeq)
	if err != nil {
		t.Fatalf("dlq getmsg: %v", err)
	}
	if want := "_dlq.default.user.created"; raw.Subject != want {
		t.Errorf("DLQ subject = %q, want %q (topic must be preserved)", raw.Subject, want)
	}
	var got wrapperspb.StringValue
	if err := proto.Unmarshal(raw.Data, &got); err != nil {
		t.Fatalf("unmarshal DLQ payload: %v", err)
	}
	if got.GetValue() != "doomed" {
		t.Errorf("DLQ payload = %q, want %q (payload must be preserved)", got.GetValue(), "doomed")
	}
}

// INVARIANT (R-bus-2): draining concurrently with active delivery is
// race-clean. The consume callback gates its per-message wg.Add on the
// draining flag (under the subscriber mutex Drain also takes), so the
// Add can never race Drain's wg.Wait across the zero counter — the
// WaitGroup-reuse panic the pre-fix callback was prone to. Run under
// `-race`; a continuous emit stream + fast handlers churn the counter
// across zero throughout the Drain.
func TestNatsBus_DrainDuringDelivery_NoWaitGroupRace_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "drainrace-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	var delivered atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error {
			delivered.Add(1) // fast → counter rapidly returns to zero between msgs
			return nil
		})

	stop := make(chan struct{})
	var pub sync.WaitGroup
	pub.Add(1)
	go func() {
		defer pub.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
			}
		}
	}()

	waitFor(t, 5*time.Second, "first deliveries", func() bool { return delivered.Load() > 0 })
	if err := sub.Drain(timeout(t, 5*time.Second)); err != nil {
		t.Errorf("Drain during active delivery: %v", err)
	}
	close(stop)
	pub.Wait()
}

// TestNatsBus_DrainOneSubscriberWhileSiblingDelivers_Embedded — Q36-bus-1.
// Draining ONE subscriber while a SIBLING keeps delivering on the same bus
// must not panic with "WaitGroup reused before Wait returned". Pre-fix both
// subscribers Add to a bus-shared WaitGroup, so subA.Drain's Wait races
// subB's per-message Add across the zero counter.
func TestNatsBus_DrainOneSubscriberWhileSiblingDelivers_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "drainsibling-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	subA, _ := bus.Subscriber(context.Background(), "chanA")
	_ = subA.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { return nil })

	subB, _ := bus.Subscriber(context.Background(), "chanB")
	var delivered atomic.Int32
	_ = subB.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { delivered.Add(1); return nil })

	stop := make(chan struct{})
	var pub sync.WaitGroup
	pub.Add(1)
	go func() {
		defer pub.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = bus.Dispatch(context.Background(), "chanB", "t", wrapperspb.String("x"))
			}
		}
	}()

	waitFor(t, 5*time.Second, "sibling deliveries", func() bool { return delivered.Load() > 0 })
	// Drain ONLY subA while subB is actively delivering — pre-fix this
	// races/panics on the shared WaitGroup.
	if err := subA.Drain(timeout(t, 5*time.Second)); err != nil {
		t.Errorf("Drain(subA) while a sibling delivers: %v", err)
	}
	close(stop)
	pub.Wait()
}

// INVARIANT (R-bus-4): Dispatch detaches from the caller's ctx, so an
// emit issued on an already-cancelled RPC ctx is still published +
// delivered (ensureStream + Publish both run on the detached ctx).
func TestNatsBus_DispatchDetachesCanceledCtx_Embedded(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "detach-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
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

// INVARIANT (V-14): the MAX_DELIVERIES advisory routes to the DLQ
// exactly ONCE no matter how many replicas run the same durable
// consumer. The advisory subject keys on the shared durable name, so a
// plain core-NATS Subscribe fans the advisory out to every replica —
// each then routes the poison message to the DLQ and fires OnDrop,
// yielding N× DLQ entries + an inflated drop metric. The per-handle
// advisory subscription is a queue subscription scoped to the durable,
// so the server hands each advisory to exactly one replica: with two
// replicas the summed OnDrop count is 1, not 2.
func TestNatsBus_MaxDeliverAdvisory_SingleReplicaRoutes_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)

	// Two buses sharing the DurablePrefix model two replicas of the
	// same service: they create/attach the same durable consumer and
	// each opens its own advisory subscription against the identical
	// advisory subject.
	const prefix = "multireplica-maxdeliveradv-subscribers"
	newReplica := func() (*recordingObserver, eventbus.Subscriber) {
		obs := &recordingObserver{}
		bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
			DSN:               url,
			DurablePrefix:     prefix,
			DefaultMaxDeliver: 2,
			DefaultAckWait:    300 * time.Millisecond,
			Observer:          obs,
		})
		if err != nil {
			t.Fatalf("NewNatsBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
		sub, err := bus.Subscriber(context.Background(), "default")
		if err != nil {
			t.Fatalf("Subscriber: %v", err)
		}
		if err := sub.Subscribe(context.Background(), "**",
			func(context.Context, string, []byte) error {
				return errors.New("always fails — drives past MaxDeliver")
			}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		return obs, sub
	}

	obsA, _ := newReplica()
	obsB, _ := newReplica()

	// A separate connection dispatches the doomed message so it's
	// load-balanced across the two replicas' shared consumer.
	pubBus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           url,
		DurablePrefix: prefix + "-pub",
	})
	if err != nil {
		t.Fatalf("NewNatsBus (publisher): %v", err)
	}
	t.Cleanup(func() { _ = pubBus.Close(timeout(t, 5*time.Second)) })

	if err := pubBus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	drops := func() int {
		_, _, _, _, da := obsA.snapshots()
		_, _, _, _, db := obsB.snapshots()
		return da + db
	}
	waitFor(t, 8*time.Second, "OnDrop from MAX_DELIVERIES advisory", func() bool {
		return drops() >= 1
	})
	// Give a (wrongly) fanned-out second advisory a chance to land.
	time.Sleep(time.Second)
	if got := drops(); got != 1 {
		t.Errorf("total OnDrop across both replicas = %d, want exactly 1 "+
			"(a plain advisory subscription routes on every replica)", got)
	}
}

// TestNatsBus_SlowSuccessNotDropped_D1_Embedded — D1. A handler whose LAST
// (MaxDeliver-th) delivery overruns AckWait but ultimately SUCCEEDS must
// not be DLQ-dropped. The server times the slow delivery out at AckWait and
// fires the MAX_DELIVERIES advisory while the handler is still running;
// without the in-flight guard that advisory routes the message to the DLQ +
// fires OnDrop, so the one message is reported BOTH dropped and delivered.
// With the guard the advisory defers to the consume path → exactly one
// OnDeliverSuccess, zero OnDrop.
func TestNatsBus_SlowSuccessNotDropped_D1_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "d1-slowsuccess",
		DefaultMaxDeliver: 2,
		DefaultAckWait:    400 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(_ context.Context, _ string, _ []byte) error {
			n := attempts.Add(1)
			if n < 2 {
				return errors.New("fail first to advance to the last delivery")
			}
			// Last delivery: overrun AckWait (so the server fires the
			// MAX_DELIVERIES advisory mid-handler), then succeed.
			time.Sleep(900 * time.Millisecond)
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("slow-but-ok"))

	waitFor(t, 8*time.Second, "the slow handler eventually succeeds", func() bool {
		_, _, ds, _, _ := obs.snapshots()
		return ds >= 1
	})
	// Give a (wrong) advisory-driven drop time to land after the success.
	time.Sleep(1 * time.Second)
	if _, _, ds, _, drop := obs.snapshots(); drop != 0 {
		t.Errorf("a message that succeeded (slowly) must not be DLQ-dropped; got drop=%d, deliverSuccess=%d (D1 incoherent drop+success)", drop, ds)
	}
}

// TestNatsBus_SlowFailureAtCapStillDLQs_D1_Embedded — D1 companion. A
// handler whose only delivery overruns AckWait then FAILS at the cap must
// still reach the DLQ. The advisory fires mid-handler and is skipped
// (in-flight), so the consume path must DLQ the poison message itself — a
// naive skip with no consume-path DLQ would silently lose it.
func TestNatsBus_SlowFailureAtCapStillDLQs_D1_Embedded(t *testing.T) {
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               startEmbeddedNATS(t),
		DurablePrefix:     "d1-slowfail",
		DefaultMaxDeliver: 1,
		DefaultAckWait:    400 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, _ := bus.Subscriber(context.Background(), "default")
	var attempts atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(_ context.Context, _ string, _ []byte) error {
			attempts.Add(1)
			time.Sleep(900 * time.Millisecond) // overrun AckWait
			return errors.New("slow failure at the delivery cap")
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("doomed-slow"))

	waitFor(t, 8*time.Second, "consume-path DLQs the slow failure at the cap", func() bool {
		_, _, _, _, drop := obs.snapshots()
		return drop >= 1
	})
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (MaxDeliver=1)", got)
	}
}
