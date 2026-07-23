package eventbus

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// MemoryBusOptions configures the MemoryBus's per-channel
// backpressure. Default (zero value) reproduces the legacy
// "unbounded goroutine fan-out" behaviour for backward
// compatibility with code that calls NewMemoryBus().
//
// Per-channel BufferSizes bounds the number of concurrent
// in-flight handler invocations for that channel. When the
// in-flight count reaches the limit, emit blocks for up to
// BlockTimeout waiting for a slot; if the timeout expires
// the event is dropped + counted in the per-channel Drops()
// map.
//
// Matches the spec's `Channel.buffer_size` semantics:
//
//   - BufferSize > 0  -> bounded queue of N concurrent
//     handlers; emit blocks ≤ BlockTimeout
//     then drops.
//   - BufferSize == 0 -> unbounded goroutine fan-out (today's
//     legacy behaviour — no backpressure).
//     Equivalent to "no buffer configured"
//     for the channel.
type MemoryBusOptions struct {
	// BufferSizes maps channel name → per-channel concurrent
	// in-flight handler invocation cap. Channels not in the
	// map (or set to zero) use the unbounded path.
	BufferSizes map[string]int

	// BlockTimeout caps how long Dispatch waits for a free
	// slot when the channel's buffer is full before dropping.
	// Zero -> 100ms (framework default per spec
	// in-memory-transport.md "block briefly then drop").
	BlockTimeout time.Duration

	// Observer receives emit / deliver / drop callbacks.
	// Nil -> NopObserver (silent no-op). Implementations
	// run on the hot path; keep them O(1) — see Observer
	// godoc.
	Observer Observer
}

// MemoryBus is the in-process implementation of both
// `Dispatcher` (emit side) and `SubscriberFactory` (subscribe
// side). One bus = one shared registry = emit goroutines
// invoke handlers registered on the same bus instance.
//
// Per spec docs/specs/eventbus/in-memory-transport.md, the bus
// is the loopback that backs `TRANSPORT_MEMORY` channels:
// emit-side serialises the envelope, spawns one goroutine per
// matching handler, each handler runs with a context detached
// from the emitting RPC's cancellation (trace headers carry
// forward, ctx cancellation does not). Drain blocks on a
// process-global WaitGroup until in-flight goroutines finish
// or the drain ctx expires.
//
// MemoryBus is safe for concurrent Dispatch + Subscribe calls.
// Subscribe should typically run at boot before any Dispatch
// fires, but registering at runtime is supported (Dispatch
// snapshots the handler slice under lock, so a concurrent
// Subscribe doesn't race the iteration).
//
// Phase 10 ships MemoryBus only; NATS / Redis Streams adapters
// (Phases 11 / 12) are parked.
type MemoryBus struct {
	mu       sync.Mutex
	handlers map[string][]registeredHandler

	// Per-channel concurrency caps (E6 backpressure). Empty
	// map / missing entry = unbounded. Populated from
	// MemoryBusOptions.BufferSizes at construction.
	bufferSizes  map[string]int
	blockTimeout time.Duration

	// semaphores caps concurrent in-flight handlers per
	// channel; allocated lazily on first Dispatch to a
	// bounded channel.
	semaphores map[string]chan struct{}

	// drops tallies dropped emits per channel for
	// observability fallback queries (`bus.Drops()`); the
	// Observer interface (E7) is the primary push surface.
	drops map[string]int64

	// observer is the metric/log callback surface. Always
	// non-nil — NopObserver wired by NewMemoryBusWithOptions
	// when caller leaves Options.Observer nil.
	observer Observer

	// wg tracks in-flight handler goroutines; drain waits on it.
	wg sync.WaitGroup

	// draining is set (under mu) once drain begins, after which
	// Dispatch stops spawning handlers. dispatchWG tracks in-flight
	// Dispatch fan-outs so drain can wait for them to finish issuing
	// their per-handler wg.Add calls BEFORE it calls wg.Wait. Together
	// these guarantee no wg.Add ever races wg.Wait across the zero
	// counter (the one pattern sync.WaitGroup forbids), which the
	// previous unguarded shutdown-during-emit path could trip.
	draining   bool
	dispatchWG sync.WaitGroup
}

// registeredHandler captures one Subscribe call's filter +
// callback. Stored per channel in the bus's handler map.
type registeredHandler struct {
	topicFilter string
	fn          HandlerFunc
}

// NewMemoryBus returns a ready bus with the legacy unbounded
// fan-out (no per-channel buffer). Equivalent to
// `NewMemoryBusWithOptions(MemoryBusOptions{})`. Kept as a
// stable shorthand for tests + simple callers that don't need
// backpressure.
func NewMemoryBus() *MemoryBus {
	return NewMemoryBusWithOptions(MemoryBusOptions{})
}

// NewMemoryBusWithOptions returns a bus with per-channel
// backpressure configured. Channels listed in
// opts.BufferSizes with positive values cap concurrent
// in-flight handler invocations; emit blocks up to
// opts.BlockTimeout when the cap is hit + drops on timeout.
// Channels missing from the map (or set to zero) use the
// unbounded path for backward compat.
func NewMemoryBusWithOptions(opts MemoryBusOptions) *MemoryBus {
	bufferSizes := map[string]int{}
	for k, v := range opts.BufferSizes {
		if v > 0 {
			bufferSizes[k] = v
		}
	}
	blockTimeout := opts.BlockTimeout
	if blockTimeout <= 0 {
		blockTimeout = 100 * time.Millisecond
	}
	observer := opts.Observer
	if observer == nil {
		observer = NopObserver{}
	}
	return &MemoryBus{
		handlers:     map[string][]registeredHandler{},
		bufferSizes:  bufferSizes,
		blockTimeout: blockTimeout,
		semaphores:   map[string]chan struct{}{},
		drops:        map[string]int64{},
		observer:     observer,
	}
}

// Drops returns a snapshot of the per-channel dropped-emit
// counter. Drops happen when a channel with a bounded buffer
// is saturated + the BlockTimeout expires waiting for a
// slot. Reset is not provided — counters monotonically grow
// for the bus's lifetime so a test or scrape consumer can
// observe deltas.
func (m *MemoryBus) Drops() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.drops))
	for k, v := range m.drops {
		out[k] = v
	}
	return out
}

// Dispatch serialises the envelope, snapshots the per-channel
// handler list under lock, then fires one goroutine per
// matching handler. Errors returned by handlers are silently
// dropped at the bus boundary (per spec emit.md's
// fire-and-forget rule). The proto.Marshal failure is the
// only error path the caller sees — typically a programmer
// mistake passing an invalid envelope.
func (m *MemoryBus) Dispatch(ctx context.Context, channel, topic string, envelope proto.Message) error {
	raw, err := proto.Marshal(envelope)
	if err != nil {
		m.observer.OnEmitFailure(channel, topic, err)
		return err
	}
	m.observer.OnEmitSuccess(channel, topic)

	m.mu.Lock()
	if m.draining {
		// Bus is draining/shutting down — drop new emits rather than
		// spawn handlers whose wg.Add would race the drain's wg.Wait.
		m.mu.Unlock()
		return nil
	}
	source := m.handlers[channel]
	snapshot := make([]registeredHandler, len(source))
	copy(snapshot, source)
	// Mark this Dispatch's fan-out in-flight under the lock so drain
	// waits for every per-handler wg.Add it is about to issue.
	m.dispatchWG.Add(1)
	m.mu.Unlock()
	defer m.dispatchWG.Done()

	// Detach from the emit ctx so handlers outlive the
	// originating RPC's cancellation. Trace + baggage carry
	// forward; cancellation does not — standard Go 1.21+
	// idiom for fire-and-forget background work that should
	// retain observability lineage.
	bgCtx := context.WithoutCancel(ctx)

	sem := m.semaphoreFor(channel)
	for _, h := range snapshot {
		if !MatchGlob(h.topicFilter, topic) {
			continue
		}
		if sem == nil {
			// Unbounded fan-out — legacy behaviour, no
			// backpressure. Spawn directly.
			m.wg.Add(1)
			go func(h registeredHandler) {
				defer m.wg.Done()
				start := time.Now()
				err := h.fn(bgCtx, topic, raw)
				reportDeliverLatency(m.observer, channel, topic, time.Since(start))
				if err != nil {
					m.observer.OnDeliverFailure(channel, topic, err)
					return
				}
				m.observer.OnDeliverSuccess(channel, topic)
			}(h)
			continue
		}
		// Bounded — try to acquire a slot. The select
		// blocks until either the semaphore admits us or
		// the timeout fires; on timeout we increment the
		// drop counter + skip this handler.
		timer := time.NewTimer(m.blockTimeout)
		select {
		case sem <- struct{}{}:
			timer.Stop()
			m.wg.Add(1)
			go func(h registeredHandler) {
				defer m.wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				err := h.fn(bgCtx, topic, raw)
				reportDeliverLatency(m.observer, channel, topic, time.Since(start))
				if err != nil {
					m.observer.OnDeliverFailure(channel, topic, err)
					return
				}
				m.observer.OnDeliverSuccess(channel, topic)
			}(h)
		case <-timer.C:
			m.recordDrop(channel)
			m.observer.OnDrop(channel, topic, DropReasonBufferFull)
		}
	}
	return nil
}

// semaphoreFor returns the per-channel concurrency semaphore,
// allocated lazily on first use, OR nil when the channel is
// unbounded (no BufferSize configured).
func (m *MemoryBus) semaphoreFor(channel string) chan struct{} {
	size, bounded := m.bufferSizes[channel]
	if !bounded || size <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sem, ok := m.semaphores[channel]
	if !ok {
		sem = make(chan struct{}, size)
		m.semaphores[channel] = sem
	}
	return sem
}

// recordDrop increments the per-channel drop counter under
// the bus mutex. Phase E7 wires this into the observability
// surface; E6 ships the counter only.
func (m *MemoryBus) recordDrop(channel string) {
	m.mu.Lock()
	m.drops[channel]++
	m.mu.Unlock()
}

// Subscriber returns a per-channel Subscriber bound to this
// bus. SubscriberFactory contract — generated code calls
// `factory.Subscriber(ctx, "<channel>")` once per channel at
// boot.
func (m *MemoryBus) Subscriber(_ context.Context, channel string) (Subscriber, error) {
	return &memorySubscriber{bus: m, channel: channel}, nil
}

// register slots a HandlerFunc into the bus under `channel`
// with the given topic filter. Concurrent calls are
// serialised by the bus mutex.
func (m *MemoryBus) register(channel, topicFilter string, h HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[channel] = append(m.handlers[channel], registeredHandler{
		topicFilter: topicFilter,
		fn:          h,
	})
}

// drain blocks until every in-flight Dispatch goroutine
// completes or `ctx` expires. Returns ctx.Err() on timeout
// so the caller can log "drain ceiling exceeded; <N> events
// dropped".
//
// All Subscriber.Drain calls funnel here — there's one
// shared WaitGroup per bus; per-Subscriber draining doesn't
// make sense when goroutines fan out across channels from a
// single emit point.
func (m *MemoryBus) drain(ctx context.Context) error {
	// Stop accepting new dispatches first, so no further wg.Add can be
	// issued once we start waiting.
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		// Wait for in-flight Dispatch fan-outs to finish issuing their
		// per-handler wg.Add calls, THEN wait on the handler WaitGroup.
		// This ordering guarantees no wg.Add races this wg.Wait.
		m.dispatchWG.Wait()
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// memorySubscriber is the per-channel Subscriber handle the
// bus hands back from Subscriber(). Stores only the
// channel name + a back-reference; all state lives on the
// shared bus.
type memorySubscriber struct {
	bus     *MemoryBus
	channel string
}

// Subscribe registers `h` for messages arriving on the
// subscriber's channel that match `topicFilter`. Returns nil
// — the in-memory transport has no failure mode at Subscribe
// time (the per-channel registration is a single map write
// under lock).
func (s *memorySubscriber) Subscribe(_ context.Context, topicFilter string, h HandlerFunc) error {
	s.bus.register(s.channel, topicFilter, recoverHandler(h))
	return nil
}

// Drain blocks until every in-flight Dispatch goroutine
// completes (shared bus-level WaitGroup) or `ctx` expires.
func (s *memorySubscriber) Drain(ctx context.Context) error {
	return s.bus.drain(ctx)
}
