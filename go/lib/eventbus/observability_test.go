package eventbus_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// recordingObserver is a test double that captures every
// Observer callback into thread-safe slices so tests can
// assert the sequence + per-event metadata. Concurrent
// Observer calls (every transport drives them off goroutines)
// stay correct via the embedded mutex.
type recordingObserver struct {
	mu sync.Mutex

	emitSuccess    []callRecord
	emitFailure    []callRecord
	deliverSuccess []callRecord
	deliverFailure []callRecord
	drop           []callRecord
}

type callRecord struct {
	Channel string
	Topic   string
	Reason  string // OnDrop only
	Err     string // OnEmit/DeliverFailure only
}

func (r *recordingObserver) OnEmitSuccess(channel, topic string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitSuccess = append(r.emitSuccess, callRecord{Channel: channel, Topic: topic})
}

func (r *recordingObserver) OnEmitFailure(channel, topic string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitFailure = append(r.emitFailure, callRecord{Channel: channel, Topic: topic, Err: err.Error()})
}

func (r *recordingObserver) OnDeliverSuccess(channel, topic string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliverSuccess = append(r.deliverSuccess, callRecord{Channel: channel, Topic: topic})
}

func (r *recordingObserver) OnDeliverFailure(channel, topic string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliverFailure = append(r.deliverFailure, callRecord{Channel: channel, Topic: topic, Err: err.Error()})
}

func (r *recordingObserver) OnDrop(channel, topic, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop = append(r.drop, callRecord{Channel: channel, Topic: topic, Reason: reason})
}

// snapshots returns the captured slice lengths for compact
// test assertions.
func (r *recordingObserver) snapshots() (int, int, int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.emitSuccess), len(r.emitFailure), len(r.deliverSuccess), len(r.deliverFailure), len(r.drop)
}

// TestMemoryBus_ObserverCallbacks — wires a recording
// observer into MemoryBus + asserts every callback fires on
// the right code path.
func TestMemoryBus_ObserverCallbacks(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		Observer: obs,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")

	var failedOnce atomic.Bool
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			// First call errs, rest succeed — drives both
			// DeliverFailure + DeliverSuccess in one test.
			if failedOnce.CompareAndSwap(false, true) {
				return errors.New("intentional handler failure")
			}
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("a"))
	_ = bus.Dispatch(context.Background(), "default", "user.deleted", wrapperspb.String("b"))
	_ = sub.Drain(timeoutCtx(t, 500*time.Millisecond))

	es, ef, ds, df, dr := obs.snapshots()
	if es != 2 {
		t.Errorf("EmitSuccess = %d, want 2", es)
	}
	if ef != 0 {
		t.Errorf("EmitFailure = %d, want 0 (no marshal failures)", ef)
	}
	if ds != 1 {
		t.Errorf("DeliverSuccess = %d, want 1", ds)
	}
	if df != 1 {
		t.Errorf("DeliverFailure = %d, want 1", df)
	}
	if dr != 0 {
		t.Errorf("OnDrop = %d, want 0 (unbounded bus)", dr)
	}

	// Per-call metadata sanity.
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.emitSuccess[0].Channel != "default" || obs.emitSuccess[0].Topic != "user.created" {
		t.Errorf("emitSuccess[0] = %+v", obs.emitSuccess[0])
	}
	if obs.deliverFailure[0].Err != "intentional handler failure" {
		t.Errorf("deliverFailure err = %q", obs.deliverFailure[0].Err)
	}
}

// TestMemoryBus_ObserverDropFires — bounded channel,
// saturation triggers OnDrop with DropReasonBufferFull.
func TestMemoryBus_ObserverDropFires(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		BufferSizes:  map[string]int{"default": 1},
		BlockTimeout: 20 * time.Millisecond,
		Observer:     obs,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")

	release := make(chan struct{})
	defer close(release)
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			<-release
			return nil
		})

	// 4 dispatches; capacity 1 + slow handler => 3 drop.
	for i := 0; i < 4; i++ {
		_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	}
	time.Sleep(120 * time.Millisecond)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if got := len(obs.drop); got != 3 {
		t.Errorf("OnDrop = %d, want 3", got)
	}
	for _, d := range obs.drop {
		if d.Reason != eventbus.DropReasonBufferFull {
			t.Errorf("drop reason = %q, want %q", d.Reason, eventbus.DropReasonBufferFull)
		}
	}
}

// TestNopObserver_SilentNoop — the default Observer (NopObserver)
// never panics + does nothing visible. Cheapest possible
// smoke test for the silent-default contract.
func TestNopObserver_SilentNoop(t *testing.T) {
	nop := eventbus.NopObserver{}
	nop.OnEmitSuccess("c", "t")
	nop.OnEmitFailure("c", "t", errors.New("e"))
	nop.OnDeliverSuccess("c", "t")
	nop.OnDeliverFailure("c", "t", errors.New("e"))
	nop.OnDrop("c", "t", "reason")
}

// TestMemoryBus_ObserverNilDefaults — wires nil Observer
// explicitly and verifies the bus uses NopObserver silently
// (no panic, normal dispatch works).
func TestMemoryBus_ObserverNilDefaults(t *testing.T) {
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		Observer: nil,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")
	called := make(chan struct{}, 1)
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			called <- struct{}{}
			return nil
		})
	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("handler never invoked with nil Observer")
	}
}

// recordingLatencyObserver embeds recordingObserver and adds
// the additive OnDeliverComplete callback. Implements
// eventbus.ObserverWithLatency.
type recordingLatencyObserver struct {
	recordingObserver

	latencyMu sync.Mutex
	latency   []latencyRecord
}

type latencyRecord struct {
	Channel  string
	Topic    string
	Duration time.Duration
}

func (r *recordingLatencyObserver) OnDeliverComplete(channel, topic string, duration time.Duration) {
	r.latencyMu.Lock()
	defer r.latencyMu.Unlock()
	r.latency = append(r.latency, latencyRecord{Channel: channel, Topic: topic, Duration: duration})
}

// TestMemoryBus_ObserverLatencyFiresOnBothPaths — wires an
// ObserverWithLatency impl into MemoryBus, drives both a
// success and a failure delivery, and asserts
// OnDeliverComplete fires exactly twice with positive
// durations. Proves the additive interface flows through
// MemoryBus's dispatch loop independently of the existing
// success/failure callbacks.
func TestMemoryBus_ObserverLatencyFiresOnBothPaths(t *testing.T) {
	obs := &recordingLatencyObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		Observer: obs,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")

	var failedOnce atomic.Bool
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error {
			// First call sleeps a hair + errs; second call
			// sleeps a hair + succeeds. Sleep is just to
			// guarantee a non-zero duration on every CPU.
			time.Sleep(2 * time.Millisecond)
			if failedOnce.CompareAndSwap(false, true) {
				return errors.New("intentional handler failure")
			}
			return nil
		})

	_ = bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("a"))
	_ = bus.Dispatch(context.Background(), "default", "user.deleted", wrapperspb.String("b"))
	_ = sub.Drain(timeoutCtx(t, 500*time.Millisecond))

	obs.latencyMu.Lock()
	defer obs.latencyMu.Unlock()
	if got := len(obs.latency); got != 2 {
		t.Fatalf("OnDeliverComplete fired %d times, want 2 (one per delivery, success + failure)", got)
	}
	for i, r := range obs.latency {
		if r.Channel != "default" {
			t.Errorf("latency[%d].Channel = %q, want default", i, r.Channel)
		}
		if r.Topic != "user.created" && r.Topic != "user.deleted" {
			t.Errorf("latency[%d].Topic = %q, want user.created or user.deleted", i, r.Topic)
		}
		if r.Duration <= 0 {
			t.Errorf("latency[%d].Duration = %v, want > 0", i, r.Duration)
		}
	}
}

// TestMemoryBus_PlainObserverSkipsLatencyHook — the
// recordingObserver impl (Observer-only, no
// ObserverWithLatency) drives the bus without triggering any
// extra cost. Proves the type-assertion fallback works: a
// plain Observer doesn't receive OnDeliverComplete and the
// bus doesn't panic looking for the method.
func TestMemoryBus_PlainObserverSkipsLatencyHook(t *testing.T) {
	obs := &recordingObserver{} // base Observer, no latency.
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		Observer: obs,
	})
	sub, _ := bus.Subscriber(context.Background(), "default")
	_ = sub.Subscribe(context.Background(), "**",
		func(ctx context.Context, topic string, raw []byte) error { return nil })

	_ = bus.Dispatch(context.Background(), "default", "t", wrapperspb.String("x"))
	_ = sub.Drain(timeoutCtx(t, 500*time.Millisecond))

	_, _, ds, _, _ := obs.snapshots()
	if ds != 1 {
		t.Errorf("DeliverSuccess = %d, want 1 (latency path must not break base hooks)", ds)
	}
}

// timeoutCtx is the codegen test pattern's
// timeout-on-cleanup helper, duplicated here so this test
// file doesn't depend on memory_test's private one.
func timeoutCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestDefaultObserver_EmitFailureLeavesATrace — the promise four documents
// make must be true of the bus a generated main actually builds.
//
// T2-6 pass #9, D9-3. `docs/specs/eventbus/emit.md` says emit failures are
// "logged + counted in observability metrics", and uses that as its
// justification for not failing the RPC; `dispatcher.go`'s header says it
// again; and every emitted helper comments that failures are "observed via
// runtime metrics, not propagated" while doing `_ = bus.Dispatch(...)`.
//
// The only failure sink Dispatch has is Observer.OnEmitFailure, every bus
// defaulted that to NopObserver, and the whole tree contained exactly two
// Observer implementations — both test recorders. So a failed dispatch
// after a committed mutation was a lost event with no trace anywhere, and
// the generated main gives a project no seam to add one: the bus is built
// inside DO-NOT-EDIT code, and no annotation, env var or hook reaches it.
//
// The test builds the bus the way the generated main does — options with no
// Observer — and asserts a failure leaves a record.
func TestDefaultObserver_EmitFailureLeavesATrace(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	bus := eventbus.NewMemoryBus()

	// An invalid-UTF-8 string field fails proto.Marshal, so Dispatch
	// fails before publish — exactly the OnEmitFailure case the spec
	// describes as "logged + counted".
	bad := wrapperspb.String(string([]byte{0xff, 0xfe}))
	if err := bus.Dispatch(context.Background(), "default", "task.created", bad); err == nil {
		t.Fatal("expected the dispatch to fail; without a failure this test proves nothing about the failure path")
	}
	if buf.Len() == 0 {
		t.Fatal("a failed emit left no trace: the spec promises it is logged and counted, the emitted helper discards the error on that promise, and the default observer is the only thing that can keep it")
	}
	if got := buf.String(); !strings.Contains(got, "task.created") {
		t.Errorf("the trace does not name the topic that was lost, so an operator cannot tell WHICH event vanished: %q", got)
	}
}

// TestNopObserver_StaysSilent — the explicit opt-out must still be silent.
// Without this, "the default traces failures" could be satisfied by making
// every observer trace, taking the opt-out away from anyone who measured
// that the hot path cannot afford it.
func TestNopObserver_StaysSilent(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: eventbus.NopObserver{}})

	_ = bus.Dispatch(context.Background(), "default", "task.created", wrapperspb.String(string([]byte{0xff, 0xfe})))
	if buf.Len() != 0 {
		t.Errorf("NopObserver is the documented silent opt-out but something logged: %q", buf.String())
	}
}
