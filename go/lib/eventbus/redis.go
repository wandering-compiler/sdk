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

	// ConsumerName — disambiguates replicas inside one
	// consumer group. Empty -> os.Hostname() (falls back to
	// "wc-subscriber" if hostname unavailable).
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
	client := b.client
	b.mu.Unlock()

	for _, s := range subs {
		_ = s.Drain(ctx)
	}
	if client != nil {
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
func (b *RedisBus) Subscriber(ctx context.Context, channel string) (Subscriber, error) {
	group := b.opts.GroupPrefix + "-" + channel
	if err := b.ensureGroup(ctx, channel, group); err != nil {
		return nil, err
	}
	sub := &redisSubscriber{
		bus:     b,
		channel: channel,
		group:   group,
	}
	b.mu.Lock()
	b.subscribers = append(b.subscribers, sub)
	b.mu.Unlock()
	return sub, nil
}

// ensureGroup creates the consumer group on the stream,
// tolerating BUSYGROUP (group already exists). MKSTREAM
// creates the stream lazily if no producer has published yet.
func (b *RedisBus) ensureGroup(ctx context.Context, channel, group string) error {
	key := channel + ":" + group
	b.mu.Lock()
	if b.groups[key] {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	err := b.client.XGroupCreateMkStream(ctx, channel, group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("xgroup create %s/%s: %w", channel, group, err)
	}

	b.mu.Lock()
	b.groups[key] = true
	b.mu.Unlock()
	return nil
}

// recreateGroup re-establishes a consumer group that disappeared under
// the bus (Redis-as-cache flush / non-persistent restart). It drops the
// stale cache entry so ensureGroup actually re-issues XGROUP CREATE
// rather than short-circuiting on the cached flag. The group is
// recreated at "$": messages that were in the stream while the group
// was gone are unrecoverable (they died with the flush), but new emits
// flow again instead of the loop spinning on NOGROUP forever.
func (b *RedisBus) recreateGroup(ctx context.Context, channel, group string) {
	key := channel + ":" + group
	b.mu.Lock()
	delete(b.groups, key)
	b.mu.Unlock()
	_ = b.ensureGroup(ctx, channel, group)
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

	// wg tracks THIS subscriber's in-flight loop goroutines +
	// per-message dispatches (Q36-bus-2). Drain waits on it, so
	// draining one subscriber never blocks on a sibling's loops —
	// a bus-shared WaitGroup deadlocked Drain (and panicked NATS on
	// the reuse race) when a second subscriber kept Add-ing.
	wg sync.WaitGroup

	// inFlight holds message IDs currently being processed by THIS
	// consumer, so the claim loop won't reclaim + reprocess a message
	// whose handler is still running past ackWait (mcpbus-sec-4). This
	// only guards self-reclaim; cross-consumer redelivery is inherent to
	// the at-least-once / competing-consumers model, so handlers with
	// external side-effects must still be idempotent.
	inFlight sync.Map // map[string]struct{}
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
	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.mu.Unlock()

	s.wg.Add(2)
	go s.consumeLoop(consumeCtx, h)
	go s.claimLoop(consumeCtx, h)
	return nil
}

// Drain cancels every consume + claim goroutine started via
// Subscribe + waits for them (and any in-flight handler
// invocations) via the bus-shared WaitGroup.
func (s *redisSubscriber) Drain(ctx context.Context) error {
	s.mu.Lock()
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
		if _, busy := s.inFlight.Load(p.ID); busy {
			continue
		}
		if p.RetryCount >= int64(maxDeliver) {
			s.routeToDLQ(ctx, p.ID)
			continue
		}
		claimed, err := s.bus.client.XClaim(ctx, &goredis.XClaimArgs{
			Stream:   s.channel,
			Group:    s.group,
			Consumer: s.bus.opts.ConsumerName,
			MinIdle:  ackWait,
			Messages: []string{p.ID},
		}).Result()
		if err != nil {
			continue
		}
		for _, msg := range claimed {
			s.processMessage(ctx, h, msg.ID, msg.Values, p.RetryCount)
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
	// Mark in-flight so the claim loop won't reclaim this message while
	// we're still processing it (mcpbus-sec-4).
	s.inFlight.Store(msgID, struct{}{})
	defer s.inFlight.Delete(msgID)

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
