package restgw_test

// Pins for the lazy-open path of the shared SSE hub
// (EventbusEventSource.ensureOpen).
//
// Two invariants, both of which the surface has broken once:
//
//  1. A FAILED open must leave nothing running. The channels opened before
//     the failing one already have live consume loops; dropping the slice on
//     the floor leaks one set per retry, and a broker outage retries once per
//     reconnecting client. (The bug this fixed; the drain is what pins it.)
//
//  2. A failed open must not WEDGE the surface. The drain that closed (1)
//     ran inside the hub's client-set lock, and both shipped adapters' Drain
//     waits for in-flight handler callbacks — whose first act is to take that
//     same lock. One transient subscribe failure coinciding with one delivery
//     in flight therefore held the lock forever: every later /w17-events
//     request and every bus delivery blocked behind it, permanently.
//
// The stub subscriber below reproduces the shipped adapters' Drain contract
// exactly: a delivery is in flight before Subscribe returns (NATS starts
// consumer.Consume BEFORE the advisory subscription that can fail), the
// in-flight callback holds a WaitGroup count, and Drain waits on that count
// bounded only by the CALLER's context. Both tests are written as invariants
// (bounded returns + observable drains), not as race-detector runs, so they
// carry their weight in the plain `make ci` lanes.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// hubTranscode is the identity transcode used by the hub pins.
func hubTranscode(_ string, envelope []byte) ([]byte, map[string]string, bool, error) {
	return envelope, nil, false, nil
}

// drainingSub mirrors the NATS/Redis subscriber lifecycle the hub drains
// against: Subscribe puts one delivery in flight before it returns (and may
// then report failure, as nats.go does when the advisory QueueSubscribe fails
// after consumer.Consume is already live), the in-flight callback holds a
// WaitGroup count, and Drain waits on it bounded only by the caller's ctx.
type drainingSub struct {
	failSubscribe bool

	wg     sync.WaitGroup
	drains atomic.Int32

	mu sync.Mutex
	h  eventbus.HandlerFunc
}

func (s *drainingSub) Subscribe(_ context.Context, _ string, h eventbus.HandlerFunc) error {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()

	// A delivery lands the instant the consume loop starts (a durable with
	// backlog from a previous boot delivers immediately).
	started := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		close(started)
		_ = h(context.Background(), "task.created", []byte(`{"in":"flight"}`))
	}()
	<-started
	// Give that delivery time to reach the hub's lock, so the interleaving is
	// deterministic rather than merely likely.
	time.Sleep(20 * time.Millisecond)

	if s.failSubscribe {
		return errors.New("injected: advisory subscribe failed after the consume loop went live")
	}
	return nil
}

func (s *drainingSub) Drain(ctx context.Context) error {
	s.drains.Add(1)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// deliver pushes one delivery through the handler the hub registered.
func (s *drainingSub) deliver(t *testing.T, topic, payload string) {
	t.Helper()
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	if h == nil {
		t.Fatal("subscriber never received a handler")
	}
	_ = h(context.Background(), topic, []byte(payload))
}

// scriptedFactory hands out drainingSubs, optionally failing one channel's
// Subscriber() outright (the multi-channel partial-failure shape) and/or the
// first N Subscribe() calls (the single-channel corpus shape).
type scriptedFactory struct {
	failChannel    string
	failSubscribes int

	mu   sync.Mutex
	made []*drainingSub
}

func (f *scriptedFactory) Subscriber(_ context.Context, channel string) (eventbus.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if channel == f.failChannel {
		return nil, errors.New("injected: transient broker failure")
	}
	s := &drainingSub{}
	if f.failSubscribes > 0 {
		s.failSubscribe = true
		f.failSubscribes--
	}
	f.made = append(f.made, s)
	return s, nil
}

func (f *scriptedFactory) sub(t *testing.T, i int) *drainingSub {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.made) {
		t.Fatalf("factory made %d subscribers, wanted #%d", len(f.made), i)
	}
	return f.made[i]
}

// INVARIANT: a subscribe failure can never leave the hub unable to serve.
// The failing Subscribe returns its error within a bound, and the NEXT
// client still opens the surface and receives deliveries — even when the
// failed attempt had a delivery in flight (the shape that wedged the lock).
func TestEventbusEventSource_FailedOpenDoesNotWedgeTheHub(t *testing.T) {
	// Single channel: the corpus shape — every generated gateway configures
	// exactly one w17-events channel.
	f := &scriptedFactory{failSubscribes: 1}
	src := restgw.NewEventbusEventSource(f, []string{"tasks"}, hubTranscode, nil)

	failed := make(chan error, 1)
	go func() {
		_, err := src.Subscribe(context.Background(), []string{"task.created"}, nil)
		failed <- err
	}()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("expected the injected subscribe error to surface")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the /w17-events surface is WEDGED: Subscribe never returned — " +
			"ensureOpen is draining an in-flight delivery while holding the lock " +
			"that same delivery needs")
	}

	// The surface still serves: the next client opens it and gets frames.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Subscribe(ctx, []string{"task.created"}, nil)
	if err != nil {
		t.Fatalf("second client could not open the hub after a failed attempt: %v", err)
	}
	f.sub(t, 1).deliver(t, "task.created", `{"id":"t-1"}`)
	select {
	case ev := <-ch:
		if ev.Topic != "task.created" {
			t.Fatalf("got topic %q, want task.created", ev.Topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached the client — the hub opened but stopped delivering")
	}
}

// INVARIANT: however many clients arrive at once, the hub opens ONE bus
// subscriber per channel. The lazy open is serialized by its own lock rather
// than by the client-set lock, so this is the property that lock has to keep:
// a second opener would double every delivery and leak the loser's handles.
func TestEventbusEventSource_ConcurrentFirstClientsOpenOnce(t *testing.T) {
	f := &scriptedFactory{}
	src := restgw.NewEventbusEventSource(f, []string{"tasks"}, hubTranscode, nil)

	const clients = 20
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := src.Subscribe(ctx, []string{"task.created"}, nil); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("client failed to subscribe: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.made) != 1 {
		t.Fatalf("%d clients opened %d bus subscribers for 1 channel; want exactly 1",
			clients, len(f.made))
	}
}

// INVARIANT (the leak dae1372ed closed): a failed open leaves NOTHING
// running. Every subscriber the attempt opened — including the one whose
// Subscribe reported the failure — is drained before the error returns.
func TestEventbusEventSource_FailedOpenDrainsWhatItOpened(t *testing.T) {
	t.Run("failing subscribe drains its own live handle", func(t *testing.T) {
		f := &scriptedFactory{failSubscribes: 1}
		src := restgw.NewEventbusEventSource(f, []string{"tasks"}, hubTranscode, nil)

		if _, err := subscribeWithin(t, src, 10*time.Second); err == nil {
			t.Fatal("expected the injected subscribe error")
		}
		if got := f.sub(t, 0).drains.Load(); got == 0 {
			t.Error("the subscriber whose Subscribe failed was never drained — " +
				"Subscriber() handed back a live handle whether or not the " +
				"subscription took, so its consume loop leaks")
		}
	})

	t.Run("partial failure drains the channels already opened", func(t *testing.T) {
		f := &scriptedFactory{failChannel: "b"}
		src := restgw.NewEventbusEventSource(f, []string{"a", "b"}, hubTranscode, nil)

		if _, err := subscribeWithin(t, src, 10*time.Second); err == nil {
			t.Fatal("expected the injected factory error")
		}
		if got := f.sub(t, 0).drains.Load(); got == 0 {
			t.Error("channel \"a\" stayed subscribed after the attempt failed — " +
				"one leaked consume loop per reconnecting client during an outage")
		}
	})
}

// subscribeWithin runs Subscribe on its own goroutine and fails the test if it
// does not return inside the budget — a wedged hub is a test failure, not a
// hung test binary.
func subscribeWithin(t *testing.T, src *restgw.EventbusEventSource, budget time.Duration) (<-chan restgw.Event, error) {
	t.Helper()
	type result struct {
		ch  <-chan restgw.Event
		err error
	}
	done := make(chan result, 1)
	go func() {
		ch, err := src.Subscribe(context.Background(), []string{"task.created"}, nil)
		done <- result{ch, err}
	}()
	select {
	case r := <-done:
		return r.ch, r.err
	case <-time.After(budget):
		t.Fatalf("Subscribe did not return within %v — the /w17-events surface is wedged", budget)
		return nil, nil
	}
}

// --- the same invariant against the REAL NATS adapter -----------------------

// failOnceFactory runs the REAL subscriber's Subscribe (real durable, real
// consume callback, real Drain) and then reports failure for the first call,
// standing in for nats.go's transient advisory-QueueSubscribe error — which
// fires AFTER consumer.Consume is already live.
type failOnceFactory struct {
	inner eventbus.SubscriberFactory

	mu     sync.Mutex
	failed bool
}

type failOnceSub struct {
	eventbus.Subscriber
	f *failOnceFactory
}

func (s *failOnceSub) Subscribe(ctx context.Context, filter string, h eventbus.HandlerFunc) error {
	if err := s.Subscriber.Subscribe(ctx, filter, h); err != nil {
		return err
	}
	s.f.mu.Lock()
	first := !s.f.failed
	s.f.failed = true
	s.f.mu.Unlock()
	if !first {
		return nil
	}
	// Let the real consume callback pick the backlogged message up and enter
	// the handler, which is where it contends for the hub's lock.
	time.Sleep(300 * time.Millisecond)
	return errors.New("injected: advisory QueueSubscribe failed")
}

func (f *failOnceFactory) Subscriber(ctx context.Context, channel string) (eventbus.Subscriber, error) {
	sub, err := f.inner.Subscriber(ctx, channel)
	if err != nil {
		return nil, err
	}
	return &failOnceSub{Subscriber: sub, f: f}, nil
}

// startEmbeddedNATS spins up an in-process JetStream server for the hub pins.
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

// INVARIANT, against the shipped NATS adapter + a real broker: a subscribe
// failure with a backlogged message already in flight must not wedge the
// surface. Nothing here is stubbed except the error return itself — the
// durable, the consume callback and Drain are the production code.
func TestEventbusEventSource_RealNatsFailedOpenDoesNotWedgeTheHub(t *testing.T) {
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           startEmbeddedNATS(t),
		DurablePrefix: "APP_GATEWAY-w17events",
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bus.Close(ctx)
	})

	// A message already in the stream when the durable attaches — the backlog
	// from a previous boot. DeliverAll (the zero value) hands it over the
	// moment Consume starts.
	if err := bus.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String("backlog")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	src := restgw.NewEventbusEventSource(
		&failOnceFactory{inner: bus}, []string{"tasks"}, hubTranscode, nil)

	if _, err := subscribeWithin(t, src, 20*time.Second); err == nil {
		t.Fatal("expected the injected subscribe error")
	} else if !strings.Contains(err.Error(), "injected") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The surface still opens and delivers.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Subscribe(ctx, []string{"task.created"}, nil)
	if err != nil {
		t.Fatalf("second client could not open the hub after a failed attempt: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String("live")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Topic != "task.created" {
			t.Fatalf("got topic %q, want task.created", ev.Topic)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no frame reached the client — the hub opened but stopped delivering")
	}
}
