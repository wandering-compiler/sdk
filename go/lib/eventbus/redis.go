package eventbus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// RedisBusOptions configures the Redis Streams-backed eventbus
// adapter. DSN is the standard `redis://[user:pass@]host:port[/db]`
// shape parsed by go-redis's `ParseURL`. GroupPrefix scopes
// every consumer-group name so multiple subscriber surfaces
// sharing one Redis don't collide; ConsumerName disambiguates
// replicas within one group (defaults to the binary's hostname).
type RedisBusOptions struct {
	// DSN — `redis://[user:pass@]host:port[/db]`. Required.
	DSN string

	// GroupPrefix — namespace applied to every XGROUP name.
	// Conventionally `<domain>-subscribers` (or with surface
	// suffix). Required.
	GroupPrefix string

	// Delivery selects competing-consumer (default) vs fan-out semantics for
	// every subscriber this bus hands out — see [DeliveryMode]. It also
	// decides where a freshly created group starts reading, because the two
	// questions have one answer per role: a work queue must not skip its
	// backlog, a live hub must not replay history.
	Delivery DeliveryMode

	// InstanceID identifies THIS PROCESS inside a DeliveryFanOut surface; it
	// is appended to the consumer-GROUP name so each replica reads the
	// stream through its own group instead of competing inside one shared
	// group. Empty -> host + PID + random (see newInstanceID). Ignored in
	// DeliveryQueue mode, where sharing one group across replicas is the
	// point — there the replicas are told apart by ConsumerName below.
	InstanceID string

	// ConsumerName — disambiguates replicas inside one
	// consumer group. Empty -> os.Hostname() (falls back to
	// "wc-subscriber" if hostname unavailable).
	//
	// It must be UNIQUE PER PROCESS within a GroupPrefix: it is half of the
	// PEL identity Redis tracks pending deliveries under, and the in-flight
	// guard that keeps a claim loop off a running handler can only span one
	// process. The hostname default is unique per container/pod, which is
	// what every generated deploy runs; two replicas of one surface on ONE
	// host (a bare-metal deploy) share it, and then either can reclaim or
	// dead-letter a message the other is still processing. Set it explicitly
	// there.
	ConsumerName string

	// DefaultMaxDeliver caps the retry count via XPENDING's
	// delivery counter; messages whose counter exceeds this
	// land on `<channel>.dlq` + are XACK'd to release the
	// origin. Zero -> 3.
	DefaultMaxDeliver int

	// DefaultAckWait — XPENDING idle threshold. Messages
	// pending longer than this in PEL are XCLAIM'd back for
	// redelivery on the next claim-loop tick. Zero -> 30s.
	DefaultAckWait time.Duration

	// ReadBatch — XREADGROUP COUNT per blocking read. Zero
	// -> 10. Tune up for high-throughput; tune down for
	// strict ordering.
	ReadBatch int64

	// Observer receives emit / deliver / drop callbacks.
	// Nil -> NopObserver (silent no-op). Drop callbacks
	// fire on DLQ routing (PEL entries past MaxDeliver).
	Observer Observer

	// ChannelRetries overrides DefaultMaxDeliver +
	// DefaultAckWait per-channel. Used by the claim loop's
	// XPENDING idle threshold + the past-MaxDeliver DLQ
	// check. Channels missing from the map fall back to
	// Default*.
	ChannelRetries map[string]ChannelRetry
}

// RedisBus implements Dispatcher + SubscriberFactory against a
// Redis Streams cluster.
//
// Channel name -> stream key. Topic is stored as a field on
// each XADD entry alongside the serialised envelope payload;
// the subscriber re-extracts topic for per-subscription
// MatchGlob filtering in the generated dispatch code.
//
// Retry / DLQ:
//
//   - XREADGROUP delivers each message once; handler errors
//     leave the entry in the consumer group's pending entries
//     list (PEL) without acking.
//   - A per-subscriber claim goroutine polls XPENDING every
//     DefaultAckWait; entries idle past that threshold get
//     XCLAIM'd + replayed through the handler.
//   - When XPENDING's RetryCount on an entry exceeds
//     DefaultMaxDeliver, the bus XADD's the payload to
//     `<channel>.dlq` (preserving topic + payload) + XACK's
//     the origin, releasing the slot.
//
// Lifecycle:
//
//   - NewRedisBus dials the broker eagerly; DSN / auth errors
//     surface at construction.
//   - Each Subscriber.Subscribe spawns one consume + one
//     claim goroutine; Drain cancels both + waits via the
//     subscriber's own WaitGroup (so draining one subscriber
//     never blocks on a sibling — Q36-bus-2).
//   - Close drains every tracked subscriber + closes the
//     go-redis client.
type RedisBus struct {
	opts   RedisBusOptions
	client *goredis.Client

	mu          sync.Mutex
	groups      map[string]bool
	closed      bool
	subscribers []*redisSubscriber

	// fanOutGroups are the per-process consumer groups this bus created in
	// DeliveryFanOut mode. Unlike a queue group — which is the surface's
	// durable position and must outlive any single process — a fan-out group
	// is meaningless the moment this process ends, and Redis has no TTL for
	// consumer groups. Close destroys them so a restart loop doesn't leave one
	// dead group per boot behind. Best-effort only: a process that dies
	// without Close (SIGKILL, OOM) still leaks its group, which costs stream
	// metadata, not deliveries.
	fanOutGroups []redisGroupRef

	// inFlight holds the messages a handler is CURRENTLY processing, keyed by
	// the PEL identity that owns them — (stream, group, consumer) — not by the
	// subscriber OBJECT that happens to be running the handler. The guard's
	// purpose is to stop a claim loop reclaiming (or dead-lettering + XACKing)
	// a message whose handler is still running, and Redis decides who "still
	// has" a message from that identity alone: two subscribers minted from one
	// bus for one channel share the group and the consumer name, so per-object
	// maps left each blind to the other's work and one could DLQ+XACK a
	// message mid-flight in its sibling — the incoherent drop+success
	// mcpbus-sec-4/R-bus-1 closed, through the guard's blind spot. Keying by
	// identity closes it for every subscriber in this process. Two PROCESSES
	// that share an identity (equal GroupPrefix + equal ConsumerName, i.e. two
	// replicas on one host with the hostname default) cannot be reconciled
	// in-memory — see [RedisBusOptions.ConsumerName].
	inFlight sync.Map // map[string]struct{} keyed by redisPELKey
}

// redisGroupRef names one consumer group on one stream.
type redisGroupRef struct {
	channel string
	group   string
}

// redisPELKey identifies one message within the consumer identity that holds
// it. Both halves matter: the same message ID under a different group/consumer
// is a DIFFERENT pending entry with its own delivery budget, and must not be
// skipped just because someone else is processing the same payload.
func redisPELKey(channel, group, consumer, msgID string) string {
	return channel + "\x00" + group + "\x00" + consumer + "\x00" + msgID
}

// NewRedisBus validates options + opens the Redis connection
// eagerly. DSN failures surface here.
func NewRedisBus(opts RedisBusOptions) (*RedisBus, error) {
	if opts.DSN == "" {
		return nil, errors.New("RedisBusOptions: DSN is empty")
	}
	if opts.GroupPrefix == "" {
		return nil, errors.New("RedisBusOptions: GroupPrefix is empty")
	}
	parsed, err := goredis.ParseURL(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("RedisBusOptions: DSN %q: %w", opts.DSN, err)
	}
	if opts.ConsumerName == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "wc-subscriber"
		}
		opts.ConsumerName = host
	}
	if opts.DefaultMaxDeliver == 0 {
		opts.DefaultMaxDeliver = 3
	}
	if opts.DefaultAckWait == 0 {
		opts.DefaultAckWait = 30 * time.Second
	}
	if opts.ReadBatch == 0 {
		opts.ReadBatch = 10
	}
	if opts.Observer == nil {
		opts.Observer = NopObserver{}
	}
	if opts.InstanceID == "" {
		opts.InstanceID = newInstanceID()
	} else {
		opts.InstanceID = sanitizeInstanceID(opts.InstanceID)
	}
	return &RedisBus{
		opts:   opts,
		client: goredis.NewClient(parsed),
		groups: map[string]bool{},
	}, nil
}

// Close drains every subscriber + closes the underlying
// go-redis client.
func (b *RedisBus) Close(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := append([]*redisSubscriber(nil), b.subscribers...)
	groups := append([]redisGroupRef(nil), b.fanOutGroups...)
	client := b.client
	b.mu.Unlock()

	for _, s := range subs {
		_ = s.Drain(ctx)
	}
	if client != nil {
		// Every subscriber is drained, so nothing in this process still reads
		// through these groups — and no OTHER process ever did, that being what
		// makes them fan-out groups. See [RedisBus.fanOutGroups].
		for _, g := range groups {
			_ = client.XGroupDestroy(ctx, g.channel, g.group).Err()
		}
		return client.Close()
	}
	return nil
}

// Dispatch XADDs one envelope to the channel's stream as
// `{topic: <topic>, payload: <raw-bytes>}`. The topic field
// lets subscribers recover the dispatch-time topic for
// per-subscription MatchGlob filtering (Redis Streams has no
// native subject hierarchy like NATS).
func (b *RedisBus) Dispatch(ctx context.Context, channel, topic string, envelope proto.Message) error {
	raw, err := proto.Marshal(envelope)
	if err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	// Detach from the caller's (cancellable) RPC ctx so a fire-and-forget
	// emit isn't lost when the originating request ctx is already cancelled
	// — mirrors MemoryBus.Dispatch (R-bus-4). Trace/baggage carry forward;
	// cancellation does not.
	pubCtx := context.WithoutCancel(ctx)
	if err := b.client.XAdd(pubCtx, &goredis.XAddArgs{
		Stream: channel,
		Values: map[string]any{
			redisFieldTopic:   topic,
			redisFieldPayload: string(raw),
		},
	}).Err(); err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	b.opts.Observer.OnEmitSuccess(channel, topic)
	return nil
}

// Subscriber returns a per-channel Subscriber. Ensures the
// consumer group exists (XGROUP CREATE MKSTREAM, tolerating
// BUSYGROUP).
//
// Refuses once the bus is closed ([ErrBusClosed]). The closed check is
// atomic with the registration it guards, so a Subscriber() racing Close
// either registers before Close's snapshot (and is drained with the rest)
// or is refused — never a live handle nothing will drain. The check is not
// redundant with "the client is closed anyway": ensureGroup short-circuits
// on the bus's groups cache, which Close does not clear, so on a channel
// used before Close the whole call is local and would have succeeded.
func (b *RedisBus) Subscriber(ctx context.Context, channel string) (Subscriber, error) {
	if b.isClosed() {
		return nil, ErrBusClosed
	}
	group := b.groupName(channel)
	if err := b.ensureGroup(ctx, channel, group, b.groupStart()); err != nil {
		return nil, err
	}
	sub := &redisSubscriber{
		bus:     b,
		channel: channel,
		group:   group,
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBusClosed
	}
	b.subscribers = append(b.subscribers, sub)
	b.mu.Unlock()
	return sub, nil
}

// isClosed reports whether Close has run. The early-out it serves saves the
// XGROUP round-trip on a cold channel; the load-bearing check is the one
// taken atomically with registration in Subscriber.
func (b *RedisBus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Redis consumer-group start positions. `redisStartBacklog` ("0") reads the
// whole retained stream; `redisStartNew` ("$") reads only what is added after
// the group is created. Which one a group gets is decided by the delivery mode
// — see groupStart.
const (
	redisStartBacklog = "0"
	redisStartNew     = "$"
)

// groupName is the consumer group one channel is read through.
//
// In DeliveryQueue mode every replica of the surface shares it: the group IS
// the work queue, and Redis hands each entry to exactly one consumer in it.
// In DeliveryFanOut mode the group is per-PROCESS (InstanceID suffix), because
// every replica must receive every message — sharing one group there makes the
// replicas compete and each event reaches the browser clients of only one of
// them (see [DeliveryFanOut]).
func (b *RedisBus) groupName(channel string) string {
	group := b.opts.GroupPrefix + "-" + channel
	if b.opts.Delivery == DeliveryFanOut {
		group += "-" + b.opts.InstanceID
	}
	return group
}

// groupStart is where a NEWLY created group starts reading — the Redis half of
// the start-position rule [natsConsumerConfig] states for NATS, and the reason
// it is derived from the same knob is that the two adapters previously
// answered it differently by accident (Redis `$` in both roles, NATS
// DeliverAll in both roles, neither written down).
//
//   - Fan-out (a live SSE hub): `$` — only what arrives after this replica
//     attaches. History replayed into a browser presents stale events as live,
//     and the group dies with the process anyway.
//   - Queue (a durable work queue): `0` — the retained backlog. A subscriber
//     surface that boots after its producer has already emitted must still
//     process that work; `$` silently dropped it, and nothing reported the
//     loss. This is what the NATS adapter has always done (DeliverAll).
//
// Only the FIRST creation of a group is affected: XGROUP CREATE on an existing
// group is a BUSYGROUP no-op, so an already-deployed surface keeps its
// position.
func (b *RedisBus) groupStart() string {
	if b.opts.Delivery == DeliveryFanOut {
		return redisStartNew
	}
	return redisStartBacklog
}

// ensureGroup creates the consumer group on the stream at `start`,
// tolerating BUSYGROUP (group already exists). MKSTREAM
// creates the stream lazily if no producer has published yet.
func (b *RedisBus) ensureGroup(ctx context.Context, channel, group, start string) error {
	key := channel + ":" + group
	b.mu.Lock()
	if b.groups[key] {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	err := b.client.XGroupCreateMkStream(ctx, channel, group, start).Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("xgroup create %s/%s: %w", channel, group, err)
	}

	b.mu.Lock()
	b.groups[key] = true
	if b.opts.Delivery == DeliveryFanOut {
		b.fanOutGroups = append(b.fanOutGroups, redisGroupRef{channel: channel, group: group})
	}
	b.mu.Unlock()
	return nil
}

// recreateGroup re-establishes a consumer group that disappeared under
// the bus (Redis-as-cache flush / non-persistent restart). It drops the
// stale cache entry so ensureGroup actually re-issues XGROUP CREATE
// rather than short-circuiting on the cached flag.
//
// Recovery deliberately ignores groupStart and always recreates at `$`, in
// BOTH delivery modes: the normal cause is a flush that took the stream with
// it (so there is nothing to replay), and in the case where the stream did
// survive, re-reading the entire retained history through a surface that is
// mid-incident is a worse failure than losing the entries the group already
// consumed. The point here is only that delivery resumes instead of the loop
// spinning on NOGROUP forever.
func (b *RedisBus) recreateGroup(ctx context.Context, channel, group string) {
	key := channel + ":" + group
	b.mu.Lock()
	delete(b.groups, key)
	b.mu.Unlock()
	_ = b.ensureGroup(ctx, channel, group, redisStartNew)
}

// redisErrBackoff is how long a consume loop pauses after a non-Nil
// XREADGROUP error before retrying, so a hard-down broker can't pin a
// CPU core in a tight reconnect/NOGROUP loop.
const redisErrBackoff = 200 * time.Millisecond

// isNoGroupErr reports whether err is Redis's NOGROUP — the consumer
// group (or its stream) no longer exists. go-redis surfaces it as a
// plain error whose message carries the NOGROUP prefix.
func isNoGroupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOGROUP")
}

// sleepCtx pauses for d unless ctx is cancelled first, so a drain isn't
// delayed by a pending error backoff.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// redisSubscriber is the Subscriber returned by
// RedisBus.Subscriber. Each Subscribe call spawns a consume +
// claim goroutine pair; Drain cancels both via the
// subscriber-owned context.
type redisSubscriber struct {
	bus     *RedisBus
	channel string
	group   string

	mu      sync.Mutex
	cancels []context.CancelFunc
	// draining is set (under mu) when Drain begins, BEFORE the consume +
	// claim loops are cancelled. Subscribe checks it (also under mu) and
	// refuses once set, which does two things at once: no loop pair is
	// spawned with a cancel func Drain has already snapshotted away (an
	// uncancellable pair that keeps consuming, then spins on the error
	// backoff once the client closes), and the wg.Add(2) below cannot start
	// from zero concurrently with Drain's wg.Wait — the WaitGroup-reuse
	// misuse the NATS adapter's identical gate already covers (R-bus-2).
	draining bool

	// wg tracks THIS subscriber's in-flight loop goroutines +
	// per-message dispatches (Q36-bus-2). Drain waits on it, so
	// draining one subscriber never blocks on a sibling's loops —
	// a bus-shared WaitGroup deadlocked Drain (and panicked NATS on
	// the reuse race) when a second subscriber kept Add-ing.
	wg sync.WaitGroup
}

// markInFlight records msgID as being held under this subscriber's PEL
// identity and returns the release func (mcpbus-sec-4 / R-bus-1). The registry
// lives on the BUS, keyed by identity — see [RedisBus.inFlight] for why the
// per-object map was the wrong scope. Cross-consumer redelivery remains
// inherent to the at-least-once / competing-consumers model, so handlers with
// external side-effects must still be idempotent.
//
// OWNERSHIP: whoever takes a message OUT OF THE BROKER marks it, and holds the
// mark until it is done with everything it took — the consume loop for its
// whole XREADGROUP batch, the claim loop for the entry it XCLAIM'd. The key is
// a plain presence bit, not a refcount, so a second marker's release cancels
// the first's; one owner per acquisition is what keeps the bit meaning "this
// process still has it".
func (s *redisSubscriber) markInFlight(msgID string) func() {
	key := redisPELKey(s.channel, s.group, s.bus.opts.ConsumerName, msgID)
	s.bus.inFlight.Store(key, struct{}{})
	return func() { s.bus.inFlight.Delete(key) }
}

// inFlightBusy reports whether a handler under this subscriber's PEL identity
// is currently processing msgID.
func (s *redisSubscriber) inFlightBusy(msgID string) bool {
	_, busy := s.bus.inFlight.Load(redisPELKey(s.channel, s.group, s.bus.opts.ConsumerName, msgID))
	return busy
}

// Subscribe starts a consume loop + a claim loop on the
// stream/group, both fed to the same HandlerFunc. The
// topicFilter is stored on the subscription (for dispatch-
// time filtering, mirroring the NATS adapter's
// catch-all-then-filter shape) but doesn't push down to
// Redis since Streams have no native filter primitive.
func (s *redisSubscriber) Subscribe(ctx context.Context, topicFilter string, h HandlerFunc) error {
	_ = topicFilter // dispatch-side MatchGlob handles per-sub filtering
	h = recoverHandler(h)
	consumeCtx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancel is stored in s.cancels and invoked by Drain(); not leaked.
	// The cancel registration, the draining check and the wg.Add are ONE
	// critical section: Drain snapshots s.cancels and then waits on the wg, so
	// splitting them lets a late Subscribe hand its cancel func to a slice
	// nobody holds any more (loops that can never be stopped) and lets its
	// Add(2) start from zero while Drain is already inside Wait.
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		cancel()
		return ErrSubscriberDraining
	}
	s.cancels = append(s.cancels, cancel)
	s.wg.Add(2)
	s.mu.Unlock()

	go s.consumeLoop(consumeCtx, h)
	go s.claimLoop(consumeCtx, h)
	return nil
}

// Drain cancels every consume + claim goroutine started via
// Subscribe + waits for them (and any in-flight handler
// invocations) via the bus-shared WaitGroup.
func (s *redisSubscriber) Drain(ctx context.Context) error {
	s.mu.Lock()
	// Set draining BEFORE snapshotting the cancels so any Subscribe that
	// starts after this point is refused rather than spawning loops this
	// drain will neither cancel nor wait for.
	s.draining = true
	cancels := append([]context.CancelFunc(nil), s.cancels...)
	s.cancels = nil
	s.mu.Unlock()

	for _, c := range cancels {
		c()
	}

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

// consumeLoop reads new entries via XREADGROUP, calls the
// handler, ACKs on success, leaves in PEL on failure (the
// claim loop picks failures up after AckWait).
func (s *redisSubscriber) consumeLoop(ctx context.Context, h HandlerFunc) {
	defer s.wg.Done()
	bgCtx := context.WithoutCancel(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		// One read+dispatch iteration, isolated so a panic in the
		// go-redis plumbing or dispatch is recovered and the loop
		// keeps consuming (the handler itself is already
		// recover-wrapped at Subscribe). `continue` → `return` from
		// the closure re-enters the for loop identically.
		func() {
			defer recoverLoopTick(ctx, "eventbus redis consumeLoop")
			streams, err := s.bus.client.XReadGroup(ctx, &goredis.XReadGroupArgs{
				Group:    s.group,
				Consumer: s.bus.opts.ConsumerName,
				Streams:  []string{s.channel, ">"},
				Block:    time.Second,
				Count:    s.bus.opts.ReadBatch,
			}).Result()
			if err != nil {
				if errors.Is(err, goredis.Nil) || ctx.Err() != nil {
					return
				}
				if isNoGroupErr(err) {
					// The consumer group vanished under us — a Redis-as-cache
					// FLUSHALL or a restart of a non-persistent Redis drops the
					// stream and its groups. The bus's groups-cache still marks
					// it present, so without this XREADGROUP would fail with
					// NOGROUP on every iteration (a 100% CPU tight spin) and
					// delivery would stall forever. Recreate it so delivery
					// resumes (R-bus-7).
					s.bus.recreateGroup(ctx, s.channel, s.group)
				}
				// Transient Redis hiccup (broker down, NOGROUP just handled,
				// etc.); back off briefly so a hard-down broker doesn't pin a
				// CPU core re-issuing the failing read in a tight loop, then
				// retry next iteration. Phase E7 (observability) wires real
				// logging here.
				sleepCtx(ctx, redisErrBackoff)
				return
			}
			// Mark the WHOLE batch in flight BEFORE dispatching any of it.
			// XREADGROUP put every one of these in the pending list at read
			// time, but they are handled one at a time — so a sibling waiting
			// its turn behind a slow handler is pending, idle past AckWait,
			// and (until this loop) invisible to the claim loop's in-flight
			// check. That is everything the claim loop needs to treat a
			// message nobody abandoned as abandoned: XCLAIM it and run the
			// handler for it concurrently, or at MaxDeliver dead-letter and
			// XACK it while this loop still goes on to deliver it
			// successfully — one message both dropped and delivered, which
			// is the incoherence R-bus-1 exists to prevent.
			//
			// This tick is the SOLE owner of the marks it takes: they are
			// released here, once, when the whole batch is done — never
			// per-message as each handler returns. Two owners of one key in an
			// unrefcounted registry means the first release unmarks it for
			// everyone, and the claim loop is walking an XPENDING snapshot
			// that still lists the whole batch — so an early release reopened
			// the window at exactly the moment it was most likely to be seen.
			// The deferred sweep also covers a panic mid-batch, so a recovered
			// tick cannot leave ids marked forever and wedge reclaiming.
			var release []func()
			for _, stream := range streams {
				for _, msg := range stream.Messages {
					release = append(release, s.markInFlight(msg.ID))
				}
			}
			defer func() {
				for _, done := range release {
					done()
				}
			}()
			for _, stream := range streams {
				for _, msg := range stream.Messages {
					s.processMessage(bgCtx, h, msg.ID, msg.Values, 0)
				}
			}
		}()
	}
}

// claimLoop polls XPENDING every AckWait for entries past the
// idle threshold; reclaims them via XCLAIM + replays through
// the handler. Entries past MaxDeliver land on
// `<channel>.dlq` + are XACK'd to release the origin slot.
//
// Resolves per-channel retry overrides at startup; the
// ticker + idle threshold + DLQ cutoff all key off the
// channel's effective MaxDeliver / AckWait.
func (s *redisSubscriber) claimLoop(ctx context.Context, h HandlerFunc) {
	defer s.wg.Done()
	maxDeliver, ackWait := s.bus.retryFor(s.channel)
	ticker := time.NewTicker(ackWait)
	defer ticker.Stop()
	bgCtx := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer recoverLoopTick(ctx, "eventbus redis claimLoop")
				s.claimPending(bgCtx, h, maxDeliver, ackWait)
			}()
		}
	}
}

// claimPending walks the PEL once. Entries past MaxDeliver
// go to DLQ; others get XCLAIM'd + replayed.
func (s *redisSubscriber) claimPending(ctx context.Context, h HandlerFunc, maxDeliver int, ackWait time.Duration) {
	pendings, err := s.bus.client.XPendingExt(ctx, &goredis.XPendingExtArgs{
		Stream: s.channel,
		Group:  s.group,
		Idle:   ackWait,
		Start:  "-",
		End:    "+",
		Count:  100,
	}).Result()
	if err != nil {
		return
	}
	for _, p := range pendings {
		// Skip a message this consumer is still processing BEFORE the
		// max-deliver/DLQ branch (mcpbus-sec-4 + R-bus-1): a message whose
		// handler is still in-flight on its FINAL retry would otherwise be
		// DLQ'd + XACK'd out from under the running handler — double-
		// processing it (handler completes AND a copy lands on the DLQ) and
		// firing a spurious OnDrop for a message that may yet succeed. Its
		// handler is just slower than ackWait, not stuck; let it finish. On
		// success it XACKs itself (no DLQ); on failure it stays in the PEL
		// and the next tick — no longer in-flight — routes it to the DLQ.
		if s.inFlightBusy(p.ID) {
			continue
		}
		// RE-CLAIM BEFORE ACTING, on both branches. XPENDING above is a
		// SNAPSHOT: by the time this loop reaches an entry — after a DLQ
		// round-trip for an earlier one, or just after the consume loop's
		// handler returned — that entry may already have been XACK'd and be
		// gone from the PEL. The in-flight mark is the only thing that hid it,
		// and the mark is released the moment its owner is done. Acting on the
		// stale row then dead-letters a message that was delivered
		// successfully: the very drop+success incoherence above, arrived at
		// from the other side.
		//
		// XCLAIM is the broker-side answer to "is this still mine, and still
		// idle": an entry that left the PEL, or whose idle was reset by a
		// sibling claim loop that got there first, yields NOTHING and is
		// skipped. The DLQ branch used to bypass it and read the snapshot as
		// fact — it is now gated exactly like the redelivery branch.
		claimed, err := s.bus.client.XClaim(ctx, &goredis.XClaimArgs{
			Stream:   s.channel,
			Group:    s.group,
			Consumer: s.bus.opts.ConsumerName,
			MinIdle:  ackWait,
			Messages: []string{p.ID},
		}).Result()
		if err != nil || len(claimed) == 0 {
			continue
		}
		if p.RetryCount >= int64(maxDeliver) {
			s.routeToDLQ(ctx, p.ID)
			continue
		}
		for _, msg := range claimed {
			// This loop is the acquirer here — it took the message out of the
			// broker, so it holds the mark for as long as it holds the
			// message, the same rule the consume loop's batch follows.
			release := s.markInFlight(msg.ID)
			s.processMessage(ctx, h, msg.ID, msg.Values, p.RetryCount)
			release()
		}
	}
}

// retryFor returns the effective (MaxDeliver, AckWait) for
// `channel`: the per-channel override when configured, else
// the bus-level defaults.
func (b *RedisBus) retryFor(channel string) (int, time.Duration) {
	return resolveChannelRetry(b.opts.ChannelRetries, channel, b.opts.DefaultMaxDeliver, b.opts.DefaultAckWait)
}

// processMessage extracts (topic, payload), invokes the
// handler, and ACKs on success. On error the entry stays in
// PEL for the claim loop to pick up.
//
// It does NOT mark the message in flight: its CALLER does, because the guard
// has to start when the message enters this process's PEL, not when its turn
// to run comes. Both callers hold the mark across the whole unit of work they
// acquired — the consume loop across its batch, the claim loop across the
// entry it XCLAIM'd (mcpbus-sec-4 / R-bus-1). Marking here as well made
// processMessage a second owner of the same unrefcounted key, and its release
// then unmarked a message the batch was still holding. Do not reintroduce it.
//
// retryCount is passed for future observability wiring (Phase
// E7) — for now unused beyond the DLQ threshold check happens
// in claimPending before this function gets the message.
func (s *redisSubscriber) processMessage(
	ctx context.Context,
	h HandlerFunc,
	msgID string,
	values map[string]any,
	_ /* retryCount */ int64,
) {
	s.wg.Add(1)
	defer s.wg.Done()

	topic, _ := values[redisFieldTopic].(string)
	payload, _ := values[redisFieldPayload].(string)
	raw := []byte(payload)

	start := time.Now()
	err := h(ctx, topic, raw)
	reportDeliverLatency(s.bus.opts.Observer, s.channel, topic, time.Since(start))
	if err != nil {
		// Don't ack; PEL retains the entry. Claim loop
		// picks it up after AckWait.
		s.bus.opts.Observer.OnDeliverFailure(s.channel, topic, err)
		return
	}
	s.bus.opts.Observer.OnDeliverSuccess(s.channel, topic)
	_ = s.bus.client.XAck(ctx, s.channel, s.group, msgID).Err()
}

// routeToDLQ XADDs the original payload to
// `<channel>.dlq` + XACKs the origin entry to free the slot.
// Tolerates errors silently — surfaces via Observer.OnDrop
// for the metric backend.
func (s *redisSubscriber) routeToDLQ(ctx context.Context, msgID string) {
	// Fetch the original entry by ID so we have the
	// topic+payload to forward.
	msgs, err := s.bus.client.XRange(ctx, s.channel, msgID, msgID).Result()
	if err != nil || len(msgs) == 0 {
		// Couldn't fetch the source entry — do NOT ack. Leaving it in the
		// PEL lets the next claim tick retry the DLQ routing. Acking here
		// would lose a poisoned message from BOTH the stream and the DLQ,
		// defeating the at-least-once / never-lose-a-poisoned-message
		// guarantee the DLQ exists to provide.
		s.bus.opts.Observer.OnDeliverFailure(s.channel, "", err)
		return
	}
	topic, _ := msgs[0].Values[redisFieldTopic].(string)
	if addErr := s.bus.client.XAdd(ctx, &goredis.XAddArgs{
		Stream: dlqStream(s.channel),
		Values: msgs[0].Values,
	}).Err(); addErr != nil {
		// DLQ write failed — leave the origin in the PEL for a retry
		// rather than acking and dropping the payload entirely.
		s.bus.opts.Observer.OnDeliverFailure(s.channel, topic, addErr)
		return
	}
	// Only now that the payload is safely in the DLQ do we ack the origin.
	_ = s.bus.client.XAck(ctx, s.channel, s.group, msgID).Err()
	s.bus.opts.Observer.OnDrop(s.channel, topic, DropReasonMaxDeliverExceeded)
}

// dlqStream composes the DLQ stream key for a channel.
// Convention: `<channel>.dlq`.
func dlqStream(channel string) string {
	return channel + ".dlq"
}

// Redis Stream field names used by both emit + consume.
// Constants kept here so the names stay consistent across the
// XADD producer side and the XREADGROUP / XCLAIM consumer
// side.
const (
	redisFieldTopic   = "topic"
	redisFieldPayload = "payload"
)
