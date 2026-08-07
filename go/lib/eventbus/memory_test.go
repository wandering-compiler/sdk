package eventbus_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// TestMemoryBus_RoundTrip emits one envelope and asserts the
// registered handler receives matching topic + the marshaled
// payload bytes.
func TestMemoryBus_RoundTrip(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}

	received := make(chan struct {
		topic string
		raw   []byte
	}, 1)
	if err := sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			received <- struct {
				topic string
				raw   []byte
			}{topic, raw}
			return nil
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	env := wrapperspb.String("hello")
	wantRaw, _ := proto.Marshal(env)

	if err := bus.Dispatch(context.Background(), "default", "user.created", env); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case got := <-received:
		if got.topic != "user.created" {
			t.Errorf("topic = %q, want %q", got.topic, "user.created")
		}
		if string(got.raw) != string(wantRaw) {
			t.Errorf("raw bytes mismatch: got %x, want %x", got.raw, wantRaw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never invoked")
	}
}

// TestMemoryBus_DrainWaitsForInflight starts a slow handler,
// dispatches, then immediately drains — drain must return
// only AFTER the handler finishes.
func TestMemoryBus_DrainWaitsForInflight(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	var handlerDone atomic.Bool
	release := make(chan struct{})
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			<-release
			handlerDone.Store(true)
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("hi"))

	drainStart := time.Now()
	drainDone := make(chan error, 1)
	go func() {
		drainDone <- sub.Drain(context.Background())
	}()

	// Give drain a moment to start blocking on the
	// WaitGroup before releasing the handler. If drain
	// returned BEFORE we release, the handler couldn't have
	// finished and the bus would have leaked.
	time.Sleep(20 * time.Millisecond)
	if handlerDone.Load() {
		t.Fatal("handler completed before release; test setup wrong")
	}
	close(release)

	select {
	case err := <-drainDone:
		if err != nil {
			t.Errorf("Drain returned err: %v", err)
		}
		if !handlerDone.Load() {
			t.Error("Drain returned but handler not done")
		}
		if elapsed := time.Since(drainStart); elapsed < 20*time.Millisecond {
			t.Errorf("Drain returned too fast (%v) — wasn't waiting on the WaitGroup", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain didn't complete")
	}
}

// TestMemoryBus_DrainTimeout dispatches a handler that blocks
// forever; drain with a short ctx timeout returns ctx.Err().
func TestMemoryBus_DrainTimeout(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	hold := make(chan struct{})
	defer close(hold) // release the goroutine at test end
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			<-hold
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("hi"))

	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := sub.Drain(drainCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain err = %v, want context.DeadlineExceeded", err)
	}
}

// TestMemoryBus_TopicFilterMatches confirms the dispatch
// honours per-subscription topic globs: a handler with
// filter `user.*` receives matching topics, drops others.
func TestMemoryBus_TopicFilterMatches(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	var got []string
	var mu sync.Mutex
	_ = sub.Subscribe(context.Background(), "user.*",
		func(ctx context.Context, topic string, raw []byte) error {
			mu.Lock()
			got = append(got, topic)
			mu.Unlock()
			return nil
		})

	for _, topic := range []string{"user.created", "user.deleted", "billing.charged", "user"} {
		_ = bus.Dispatch(context.Background(), "default", topic, wrapperspb.String("x"))
	}

	_ = sub.Drain(timeout(t, 500*time.Millisecond))

	mu.Lock()
	defer mu.Unlock()
	wantSet := map[string]bool{"user.created": true, "user.deleted": true}
	if len(got) != 2 {
		t.Fatalf("got %d invocations, want 2; got=%v", len(got), got)
	}
	for _, topic := range got {
		if !wantSet[topic] {
			t.Errorf("unexpected topic %q matched user.*", topic)
		}
	}
}

// TestMemoryBus_MultipleSubscribersPerChannel — two handlers
// registered on the same channel both fire for a matching
// emit.
func TestMemoryBus_MultipleSubscribersPerChannel(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	var a, b atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			a.Add(1)
			return nil
		})
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			b.Add(1)
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	_ = sub.Drain(timeout(t, 500*time.Millisecond))

	if a.Load() != 1 || b.Load() != 1 {
		t.Errorf("handlers fired: a=%d b=%d; want both 1", a.Load(), b.Load())
	}
}

// TestMemoryBus_ChannelIsolation — handler on `default` is
// NOT invoked when emit happens on `audit`.
func TestMemoryBus_ChannelIsolation(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	defaultSub, _ := bus.Subscriber(context.Background(), "default")
	auditSub, _ := bus.Subscriber(context.Background(), "audit")

	var defaultCalls, auditCalls atomic.Int32
	_ = defaultSub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			defaultCalls.Add(1)
			return nil
		})
	_ = auditSub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			auditCalls.Add(1)
			return nil
		})

	_ = bus.Dispatch(context.Background(), "audit", "billing.audit", wrapperspb.String("x"))
	_ = auditSub.Drain(timeout(t, 500*time.Millisecond))

	if defaultCalls.Load() != 0 {
		t.Errorf("default channel handler fired on audit emit (%d times)", defaultCalls.Load())
	}
	if auditCalls.Load() != 1 {
		t.Errorf("audit handler fired %d times, want 1", auditCalls.Load())
	}
}

// TestMemoryBus_ContextDetached — emit with a cancelled ctx;
// handler ctx is still live (cancellation doesn't propagate
// across the WithoutCancel boundary).
func TestMemoryBus_ContextDetached(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	handlerCtxErr := make(chan error, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			handlerCtxErr <- ctx.Err()
			return nil
		})

	emitCtx, emitCancel := context.WithCancel(context.Background())
	emitCancel() // cancel BEFORE Dispatch
	_ = bus.Dispatch(emitCtx, "default", "t", wrapperspb.String("x"))

	select {
	case err := <-handlerCtxErr:
		if err != nil {
			t.Errorf("handler received cancelled ctx (err = %v); WithoutCancel should have detached", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler never fired")
	}
}

// TestMemoryBus_EmptyChannel — Dispatch to a channel with no
// subscribers succeeds silently (no error, no panic).
func TestMemoryBus_EmptyChannel(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	if err := bus.Dispatch(context.Background(), "ghost", "t", wrapperspb.String("x")); err != nil {
		t.Errorf("Dispatch to empty channel returned err: %v", err)
	}
}

// TestMemoryBus_ConcurrentSubscribeAndDispatch stresses the
// per-channel handler slice lock: register handlers while
// dispatching, assert no race detector trips + no panics.
// Detection relies on `go test -race`.
func TestMemoryBus_ConcurrentSubscribeAndDispatch(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sub.Subscribe(context.Background(), "**",
				func(ctx context.Context, topic string, raw []byte) error {
					return nil
				})
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
		}()
	}
	wg.Wait()
	_ = sub.Drain(timeout(t, time.Second))
}

// TestMemoryBus_BufferUnboundedDefaults — NewMemoryBus() (zero
// opts) retains the legacy unbounded-goroutine fan-out;
// emits never drop regardless of handler latency.
func TestMemoryBus_BufferUnboundedDefaults(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, _ := bus.Subscriber(context.Background(), "default")

	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			calls.Add(1)
			<-release
			return nil
		})

	for i := 0; i < 10; i++ {
		_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	}
	// Wait a moment for goroutines to start.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 10 {
		t.Errorf("calls = %d, want 10 (no backpressure on default bus)", got)
	}
	if drops := bus.Drops(); len(drops) != 0 {
		t.Errorf("drops should be empty on unbounded bus, got %v", drops)
	}
}

// TestMemoryBus_BufferDropsAfterTimeout — bounded channel
// saturated by slow handlers + further emits trigger drops
// after BlockTimeout. Verifies the per-channel Drops()
// counter.
func TestMemoryBus_BufferDropsAfterTimeout(t *testing.T) {
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		BufferSizes:  map[string]int{"default": 2},
		BlockTimeout: 30 * time.Millisecond,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")

	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			calls.Add(1)
			<-release
			return nil
		})

	// First two dispatches saturate the buffer (handlers
	// block on `release`).
	for i := 0; i < 5; i++ {
		_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	}
	// Give the BlockTimeout time to fire on the 3 dispatches
	// that couldn't acquire a slot.
	time.Sleep(150 * time.Millisecond)

	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 (capacity saturated, rest dropped)", got)
	}
	drops := bus.Drops()
	if got := drops["default"]; got != 3 {
		t.Errorf("drops[default] = %d, want 3", got)
	}
}

// TestMemoryBus_BufferAcceptsUnderCapacity — emits within
// capacity all succeed, no drops.
func TestMemoryBus_BufferAcceptsUnderCapacity(t *testing.T) {
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		BufferSizes:  map[string]int{"default": 5},
		BlockTimeout: 30 * time.Millisecond,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")

	var calls atomic.Int32
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			calls.Add(1)
			return nil
		})

	// 3 emits, capacity 5 — all should accept.
	for i := 0; i < 3; i++ {
		_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	}
	_ = sub.Drain(timeout(t, 500*time.Millisecond))

	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (all under capacity)", got)
	}
	if got := bus.Drops()["default"]; got != 0 {
		t.Errorf("drops should be 0, got %d", got)
	}
}

// TestMemoryBus_BufferPerChannelIsolation — bounded
// "telemetry" channel + unbounded "default" channel; emits
// on default never drop even when telemetry is saturated.
func TestMemoryBus_BufferPerChannelIsolation(t *testing.T) {
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		BufferSizes:  map[string]int{"telemetry": 1},
		BlockTimeout: 20 * time.Millisecond,
	})

	telemetrySub, _ := bus.Subscriber(context.Background(), "telemetry")
	defaultSub, _ := bus.Subscriber(context.Background(), "default")

	release := make(chan struct{})
	defer close(release)
	var telemetryCalls, defaultCalls atomic.Int32
	_ = telemetrySub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			telemetryCalls.Add(1)
			<-release
			return nil
		})
	_ = defaultSub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			defaultCalls.Add(1)
			return nil
		})

	// Saturate telemetry + flood default — only telemetry
	// should drop.
	for i := 0; i < 4; i++ {
		_ = bus.Dispatch(context.Background(), "telemetry", "t", wrapperspb.String("x"))
	}
	for i := 0; i < 4; i++ {
		_ = bus.Dispatch(context.Background(), "default", "d", wrapperspb.String("y"))
	}
	time.Sleep(100 * time.Millisecond)

	if got := defaultCalls.Load(); got != 4 {
		t.Errorf("defaultCalls = %d, want 4 (unbounded channel)", got)
	}
	if drops := bus.Drops(); drops["default"] != 0 {
		t.Errorf("default channel should not drop, got %v", drops)
	}
	if drops := bus.Drops(); drops["telemetry"] == 0 {
		t.Errorf("telemetry channel should record drops (saturated), got %v", drops)
	}
}

// timeout returns a context.Background with a deadline,
// scoped to t.Cleanup so leaked subtests don't leak goroutines.
func timeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
