package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// NatsBusOptions configures the JetStream-backed eventbus
// adapter. DSN points at the NATS server (`nats://host:port`,
// optional `user:pass@`); DurablePrefix scopes the JetStream
// durable consumer names so multiple subscriber surfaces /
// projects sharing one broker don't collide. DefaultRetry
// flows into every consumer's MaxDeliver + AckWait; per-event
// override (carried by parser.ResolvedRetry) is plumbed in a
// future slice when the Subscriber interface grows a retry
// arg.
type NatsBusOptions struct {
	// DSN — `nats://[user:pass@]host:port[,host:port,...]`.
	// Empty rejects with a descriptive error at New time.
	DSN string

	// DurablePrefix — namespace applied to every JetStream
	// durable consumer name. Conventionally
	// `<domain>-subscribers` (or `<domain>-subscribers-<surface>`).
	// Empty rejects.
	DurablePrefix string

	// Delivery selects competing-consumer (default) vs fan-out semantics for
	// every subscriber this bus hands out — see [DeliveryMode]. It also
	// decides the consumer's start position, because the two questions have
	// one answer per role: a work queue must not skip its backlog, a live
	// hub must not replay history.
	Delivery DeliveryMode

	// InstanceID identifies THIS PROCESS inside a DeliveryFanOut surface; it
	// is appended to the consumer name so each replica owns its own
	// ephemeral consumer instead of competing for one durable. Empty -> host
	// + PID + random (see newInstanceID). Ignored in DeliveryQueue mode,
	// where sharing one durable across replicas is the point.
	InstanceID string

	// DefaultMaxDeliver — bus-wide retry ceiling applied to
	// every Subscribe call. Zero -> 3 (framework default).
	DefaultMaxDeliver int

	// DefaultAckWait — per-attempt timeout. Zero -> 30s
	// (framework default).
	DefaultAckWait time.Duration

	// Observer receives emit / deliver callbacks. Nil ->
	// NopObserver (silent no-op).
	Observer Observer

	// ChannelRetries overrides DefaultMaxDeliver +
	// DefaultAckWait per-channel. Channel name -> retry
	// override. Channels missing from the map use the
	// Default* values. Codegen-generated wiring (per
	// docs/archive/eventbus-extensions-plan.md P2.2) fills this from
	// the parser's per-event ResolvedRetry rolled up to
	// channel granularity.
	ChannelRetries map[string]ChannelRetry
}

// NatsBus implements Dispatcher + SubscriberFactory against a
// NATS JetStream cluster.
//
// Channel name → JetStream stream name (lazy-created with a
// `<channel>.>` subject filter on first use). Topic → subject
// suffix (emit on channel `default` with topic `user.created`
// publishes to subject `default.user.created`). Subscriber
// uses subject filter `<channel>.>` (catch-all) + relies on
// the generated dispatch code's MatchGlob for per-subscription
// topic filtering — keeps the per-channel consumer count at
// one regardless of subscription fan-out.
//
// Retry / DLQ are driven by JetStream consumer config:
// `MaxDeliver` from NatsBusOptions.DefaultMaxDeliver,
// `AckWait` from .DefaultAckWait. When a message exhausts
// MaxDeliver the server fires a MAX_DELIVERIES advisory; the
// per-handle advisory subscription forwards the original payload
// to the channel's dead-letter stream (`<channel>_dlq`, subjects
// `_dlq.<channel>[.<topic>]`) and fires Observer.OnDrop — parity
// with the Redis adapter's `<channel>.dlq` routing (R-bus-3).
type NatsBus struct {
	opts NatsBusOptions

	mu      sync.Mutex
	conn    *natsgo.Conn
	js      jetstream.JetStream
	streams map[string]bool

	closed      bool
	subscribers []*natsSubscriber

	// claims is the lazily-created KV bucket backing the poison-outcome
	// claims (see claimPoisonOutcome). Guarded by mu for init; the handle
	// itself is safe for concurrent use.
	claims jetstream.KeyValue

	// inFlight holds the stream sequences a handler is CURRENTLY processing,
	// keyed by the CONSUMER identity that owns the delivery — (stream,
	// consumer, sequence) — not by the subscriber OBJECT running the handler.
	// The consumer name is derived from (DurablePrefix, channel, topicFilter,
	// and in fan-out mode InstanceID), so two subscribers minted from one bus
	// for one channel share it and share the advisory queue group that feeds
	// off it; per-object maps left each blind to the other's work, and one
	// could dead-letter a message its sibling was still processing — the
	// incoherent drop+success D1 closed, through the guard's blind spot. The
	// consumer stays in the key because a DIFFERENT consumer on the same
	// stream has its own delivery budget: its exhausted message must still
	// reach the DLQ, not be skipped because someone else is holding the same
	// sequence.
	//
	// A-P9-1: this map is a process-LOCAL fast path only. It cannot see a
	// handler mid-run on another replica (the advisory queue group is
	// cluster-wide), and it is not yet set for deliveries parked in the
	// client prefetch buffer. The load-bearing, replica-agnostic guard is
	// the broker-shared poison-outcome claim — see claimPoisonOutcome.
	inFlight sync.Map // map[string]struct{} keyed by natsInFlightKey
}

// natsInFlightKey identifies one delivery within the consumer identity that
// owns it. See [NatsBus.inFlight] for why both the consumer and the sequence
// are part of it.
func natsInFlightKey(stream, consumer string, seq uint64) string {
	return stream + "\x00" + consumer + "\x00" + strconv.FormatUint(seq, 10)
}

// NewNatsBus validates the options + builds the bus. The NATS
// connection is opened eagerly so DSN / auth failures surface
// at construction rather than at first Dispatch / Subscribe.
//
// Caller is responsible for calling Close() (or letting
// each Subscriber.Drain happen first) so the underlying
// connection flushes cleanly.
func NewNatsBus(opts NatsBusOptions) (*NatsBus, error) {
	if opts.DSN == "" {
		return nil, errors.New("NatsBusOptions: DSN is empty")
	}
	if opts.DurablePrefix == "" {
		return nil, errors.New("NatsBusOptions: DurablePrefix is empty")
	}
	if !strings.HasPrefix(opts.DSN, "nats://") && !strings.HasPrefix(opts.DSN, "tls://") {
		return nil, fmt.Errorf("NatsBusOptions: DSN %q must use nats:// or tls:// scheme", opts.DSN)
	}
	if opts.DefaultMaxDeliver == 0 {
		opts.DefaultMaxDeliver = 3
	}
	if opts.DefaultAckWait == 0 {
		opts.DefaultAckWait = 30 * time.Second
	}
	if opts.Observer == nil {
		opts.Observer = DefaultObserver{}
	}
	if opts.InstanceID == "" {
		opts.InstanceID = newInstanceID()
	} else {
		opts.InstanceID = sanitizeInstanceID(opts.InstanceID)
	}

	nc, err := natsgo.Connect(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", opts.DSN, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	return &NatsBus{
		opts:    opts,
		conn:    nc,
		js:      js,
		streams: map[string]bool{},
	}, nil
}

// Close drains every subscriber + closes the underlying NATS
// connection. Safe to call multiple times.
func (b *NatsBus) Close(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := append([]*natsSubscriber(nil), b.subscribers...)
	conn := b.conn
	b.mu.Unlock()

	for _, s := range subs {
		_ = s.Drain(ctx)
	}
	if conn != nil {
		conn.Close()
	}
	return nil
}

// Dispatch publishes one envelope to JetStream on the
// `<channel>.<topic>` subject. Returns the publish error
// directly; the generated emit interceptor wraps the call in
// `_ = bus.Dispatch(...)` per fire-and-forget semantics.
func (b *NatsBus) Dispatch(ctx context.Context, channel, topic string, envelope proto.Message) error {
	// Detach from the caller's (cancellable) RPC ctx so a fire-and-forget
	// emit isn't lost when the originating request ctx is already cancelled
	// — mirrors MemoryBus.Dispatch (R-bus-4). Trace/baggage carry forward;
	// cancellation does not.
	pubCtx := context.WithoutCancel(ctx)
	if err := b.ensureStream(pubCtx, channel); err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	subject := natsSubject(channel, topic)
	if _, err = b.js.Publish(pubCtx, subject, raw); err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	b.opts.Observer.OnEmitSuccess(channel, topic)
	return nil
}

// Subscriber returns a per-channel Subscriber bound to this
// bus. SubscriberFactory contract.
//
// Refuses once the bus is closed ([ErrBusClosed]). The closed check is
// atomic with the registration it guards, so a Subscriber() racing Close
// either registers before Close's snapshot (and is drained with the rest)
// or is refused — it can never hand back a live handle nothing will drain.
// The check is not redundant with "the connection is closed anyway":
// ensureStream short-circuits on the bus's stream cache, which Close does
// not clear, so on a channel used before Close the whole call is local and
// would have succeeded.
func (b *NatsBus) Subscriber(ctx context.Context, channel string) (Subscriber, error) {
	if b.isClosed() {
		return nil, ErrBusClosed
	}
	if err := b.ensureStream(ctx, channel); err != nil {
		return nil, err
	}
	sub := &natsSubscriber{bus: b, channel: channel}
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
// broker round-trip on a cold channel; the load-bearing check is the one
// taken atomically with registration in Subscriber.
func (b *NatsBus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// ensureStream creates or updates the JetStream stream backing
// `channel`. Idempotent — first call materialises, subsequent
// calls short-circuit via the bus's `streams` map.
func (b *NatsBus) ensureStream(ctx context.Context, channel string) error {
	b.mu.Lock()
	if b.streams[channel] {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	cfg := jetstream.StreamConfig{
		Name:      channel,
		Subjects:  []string{channel + ".>"},
		Retention: jetstream.LimitsPolicy,
	}
	_, err := b.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ensure stream %q: %w", channel, err)
	}

	b.mu.Lock()
	b.streams[channel] = true
	b.mu.Unlock()
	return nil
}

// ensureDLQStream materialises the dead-letter stream that
// receives messages a consumer drops past MaxDeliver (R-bus-3).
// Its subjects (`_dlq.<channel>` + `_dlq.<channel>.>`) live in a
// namespace disjoint from the main stream's `<channel>.>` filter,
// so DLQ'd messages are NOT re-consumed by the same durable (which
// would loop forever). Idempotent via the bus `streams` map, keyed
// on the DLQ stream name.
func (b *NatsBus) ensureDLQStream(ctx context.Context, channel string) error {
	name := natsDLQStreamName(channel)
	b.mu.Lock()
	if b.streams[name] {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	base, wildcard := natsDLQSubjects(channel)
	cfg := jetstream.StreamConfig{
		Name:      name,
		Subjects:  []string{base, wildcard},
		Retention: jetstream.LimitsPolicy,
	}
	if _, err := b.js.CreateOrUpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("ensure dlq stream %q: %w", name, err)
	}

	b.mu.Lock()
	b.streams[name] = true
	b.mu.Unlock()
	return nil
}

// natsDLQClaimBucket / natsDLQClaimTTL configure the KV bucket that backs the
// poison-outcome claims (claimPoisonOutcome). One bucket per broker: keys are
// scoped by (channel, consumer-name), and consumer names embed the bus's
// DurablePrefix (+ InstanceID in fan-out mode), so surfaces sharing a broker
// cannot collide inside it. The TTL garbage-collects claims — they are only
// live during the advisory↔delivery race around one message's delivery cap
// (an AckWait-scale window; 1h is generously past any realistic buffer
// dwell). A zombie buffered delivery starting later than the TTL could re-run
// a message the advisory dead-lettered — accepted as degenerate.
const (
	natsDLQClaimBucket = "w17_eventbus_dlq_claims"
	natsDLQClaimTTL    = time.Hour
)

// Claim roles, stored as the claim value for operator debuggability only —
// the decision protocol never reads them back.
const (
	natsDLQClaimRoleDelivery = "delivery"
	natsDLQClaimRoleAdvisory = "advisory"
)

// ensureDLQClaims materialises (or attaches to) the poison-outcome claim
// bucket. Idempotent; the handle is cached on the bus.
func (b *NatsBus) ensureDLQClaims(ctx context.Context) (jetstream.KeyValue, error) {
	b.mu.Lock()
	if b.claims != nil {
		kv := b.claims
		b.mu.Unlock()
		return kv, nil
	}
	b.mu.Unlock()

	kv, err := b.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      natsDLQClaimBucket,
		TTL:         natsDLQClaimTTL,
		Description: "w17 eventbus: per-(channel,consumer,seq) poison-outcome claims (advisory vs delivery DLQ authority)",
	})
	if err != nil {
		return nil, fmt.Errorf("ensure dlq claim bucket %q: %w", natsDLQClaimBucket, err)
	}

	b.mu.Lock()
	if b.claims == nil {
		b.claims = kv
	} else {
		kv = b.claims
	}
	b.mu.Unlock()
	return kv, nil
}

// natsDLQClaimKey identifies one message's poison outcome within the consumer
// identity that owns its delivery budget — same scoping as natsInFlightKey,
// dot-separated because KV keys forbid NUL (channel and consumer names are
// dot-free by NATS naming rules, so the encoding is unambiguous).
func natsDLQClaimKey(channel, consumer string, seq uint64) string {
	return channel + "." + consumer + "." + strconv.FormatUint(seq, 10)
}

// natsSubscriber is the Subscriber returned by
// NatsBus.Subscriber. One per (bus, channel) pair; the bus
// tracks them for Close-time drain.
type natsSubscriber struct {
	bus     *NatsBus
	channel string

	mu      sync.Mutex
	handles []*natsConsumeHandle
	// wg tracks THIS subscriber's in-flight message callbacks; Drain
	// waits on it. Per-subscriber (not bus-shared) so draining one
	// subscriber never races a SIBLING's wg.Add — a bus-shared
	// WaitGroup panicked with "reused before Wait returned" when a
	// second subscriber kept delivering during the first's drain
	// (Q36-bus-1).
	wg sync.WaitGroup
	// draining is set (under mu) when Drain begins, BEFORE the consume
	// handles are Stop()'d. The Consume callback checks it (also under
	// mu) and short-circuits without a wg.Add once draining — so THIS
	// subscriber's per-message Add can't race its own Drain's wg.Wait
	// across the zero counter (the WaitGroup-reuse panic R-bus-2).
	draining bool
}

// markInFlight records a stream sequence as being processed under (channel,
// durable) and returns the release func (D1 — parity with the Redis adapter's
// R-bus-1 guard). The MAX_DELIVERIES advisory can fire while a slow handler on
// the message's last delivery is still running (it overran AckWait); routing
// that message to the DLQ then would drop + OnDrop a message that may yet
// succeed (the handler later Acks + OnDeliverSuccess) — an incoherent
// drop/success for one message. The advisory skips an in-flight sequence and
// hands the DLQ decision to the consume path, which DLQs the message itself
// iff it ultimately fails at the delivery cap (the one-shot advisory has no
// second chance, unlike the Redis poller's next tick). The registry lives on
// the BUS, keyed by consumer identity — see [NatsBus.inFlight]; it is the
// same-process fast path in front of the broker-shared poison-outcome claim
// (A-P9-1, claimPoisonOutcome), which is what actually holds across replicas
// and prefetch buffers.
func (s *natsSubscriber) markInFlight(consumer string, seq uint64) func() {
	key := natsInFlightKey(s.channel, consumer, seq)
	s.bus.inFlight.Store(key, struct{}{})
	return func() { s.bus.inFlight.Delete(key) }
}

// inFlightBusy reports whether a handler under (channel, consumer) is currently
// processing seq.
func (s *natsSubscriber) inFlightBusy(consumer string, seq uint64) bool {
	_, busy := s.bus.inFlight.Load(natsInFlightKey(s.channel, consumer, seq))
	return busy
}

// claimPoisonOutcome atomically claims the right to decide the TERMINAL
// outcome of a poison message (one at its delivery cap) for (channel,
// consumer, seq): first claimant owns it, via KV Create (create-if-absent).
// Returns whether THIS caller won the claim.
//
// A-P9-1: the in-flight sync.Map alone cannot make the DLQ decision safe —
// it is process-local (a MAX_DELIVERIES advisory rides a cluster-wide queue
// group and can land on a replica that never marked the message), and it is
// not yet set while the message sits prefetched in the client buffer with
// the server's AckWait clock running. The broker offers no terminal-state
// oracle to re-check either: at the instant the advisory fires the server
// has ALREADY removed the message from ack-pending and moved the ack floor
// past it (hasMaxDeliveries), a later ack is void and leaves no per-message
// trace (only the aggregate NumRedelivered, masked to 0 for MaxDeliver=1),
// and under LimitsPolicy GetMsg happily serves an already-succeeded message
// — verified against nats-server v2.14.2 source + an embedded-server probe.
// So the DECISION itself is made atomic instead: the delivery path claims
// before running a final-attempt handler, the advisory path claims before
// dead-lettering, and the loser defers entirely.
func (s *natsSubscriber) claimPoisonOutcome(ctx context.Context, consumer string, seq uint64, role string) (bool, error) {
	kv, err := s.bus.ensureDLQClaims(ctx)
	if err != nil {
		return false, err
	}
	key := natsDLQClaimKey(s.channel, consumer, seq)
	var owned bool
	err = retryBrokerOp(ctx, natsDLQRetryAttempts, natsDLQRetryBackoff, func() error {
		_, cerr := kv.Create(ctx, key, []byte(role))
		if errors.Is(cerr, jetstream.ErrKeyExists) {
			owned = false
			return nil
		}
		if cerr != nil {
			return cerr
		}
		owned = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("nats dlq: claim poison outcome %q: %w", key, err)
	}
	return owned, nil
}

// natsFanOutInactiveThreshold is how long the server keeps a fan-out
// consumer that has stopped pulling. It only has to outlive a transient
// disconnect: the consumer belongs to ONE process and its state (which
// messages that process has seen) is worthless to anyone else, so the server
// reaps it shortly after the replica dies. Without a threshold every restarted
// replica would leave a consumer behind forever.
//
// Five minutes is chosen against the client's own reconnect budget, not
// guessed: nats.go gives up after DefaultMaxReconnect (60) × DefaultReconnectWait
// (2s) ≈ 2 minutes and closes the connection, so an outage long enough to reap
// this consumer has already taken the whole bus down with it — the threshold
// cannot be the thing that ends delivery on a link that recovered.
const natsFanOutInactiveThreshold = 5 * time.Minute

// natsConsumerConfig builds the JetStream consumer for one Subscribe, and is
// the single place the two delivery modes differ on NATS:
//
//   - DeliveryQueue → a DURABLE consumer shared by every replica of the
//     surface, starting at DeliverAll. Durable so the surface's position
//     survives a restart, shared so the broker balances the work across
//     replicas, DeliverAll so a surface deployed after its producer still
//     processes what was emitted meanwhile. (DeliverAll was already the
//     behaviour — as the zero value of an unset field. Stated here because
//     the Redis side had to be told the same thing explicitly, and a policy
//     nobody writes down is a policy that diverges.)
//
//   - DeliveryFanOut → an EPHEMERAL consumer named after this process (no
//     Durable), starting at DeliverNew. Ephemeral because the state it tracks
//     is "what this replica's browser connections have seen" — meaningless to
//     the next process, and the server reaps it after
//     natsFanOutInactiveThreshold. Per-process because every replica must
//     receive every message (see [DeliveryFanOut]). DeliverNew because a hub
//     is a LIVE surface: what a client missed while disconnected is not news,
//     and replaying a channel's history into a browser presents stale events
//     as if they had just happened. It also matches Redis's `$`, so the two
//     adapters no longer answer this question differently.
func natsConsumerConfig(mode DeliveryMode, name, channel string, maxDeliver int, ackWait time.Duration) jetstream.ConsumerConfig {
	cfg := jetstream.ConsumerConfig{
		FilterSubject: channel + ".>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    maxDeliver,
		AckWait:       ackWait,
	}
	if mode == DeliveryFanOut {
		cfg.Name = name
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
		cfg.InactiveThreshold = natsFanOutInactiveThreshold
		return cfg
	}
	cfg.Durable = name
	cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	return cfg
}

// natsConsumeHandle bundles the per-Subscribe state owned by
// a natsSubscriber: the JetStream Consume context plus the
// core-NATS subscription on the
// `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer>`
// advisory subject that drives Observer.OnDrop for
// past-MaxDeliver messages (the server publishes one
// advisory per drop, so the surfacing is push-based + topic-
// agnostic — JetStream's advisory payload carries
// stream / consumer / stream_seq but not the original
// subject).
type natsConsumeHandle struct {
	cctx        jetstream.ConsumeContext
	advisorySub *natsgo.Subscription
}

// Subscribe creates (or updates) a JetStream consumer with a
// catch-all subject filter on the channel + starts streaming
// envelopes through `h`. Each delivery's subject suffix (topic)
// is recovered + passed to the handler; on handler success the
// message is Ack'd, on failure Nack'd (NATS redelivers per the
// consumer's retry policy).
//
// The consumer name is `<DurablePrefix>-<channel>-<hash(topicFilter)>`
// (generator-side Subscribe uses `**` for every surface today, so this
// collapses to one per surface+channel) — plus, in DeliveryFanOut mode, the
// bus's per-process InstanceID. See natsConsumerConfig for what the mode
// changes and why.
func (s *natsSubscriber) Subscribe(ctx context.Context, topicFilter string, h HandlerFunc) error {
	if s.isDraining() {
		return ErrSubscriberDraining
	}
	h = recoverHandler(h)
	consumerName := s.bus.opts.DurablePrefix + "-" + s.channel
	if topicFilter != "" && topicFilter != "**" {
		consumerName += "-" + sanitizeDurableSuffix(topicFilter)
	}
	if s.bus.opts.Delivery == DeliveryFanOut {
		consumerName += "-" + s.bus.opts.InstanceID
	}

	maxDeliver, ackWait := resolveChannelRetry(s.bus.opts.ChannelRetries, s.channel, s.bus.opts.DefaultMaxDeliver, s.bus.opts.DefaultAckWait)

	consumer, err := s.bus.js.CreateOrUpdateConsumer(ctx, s.channel,
		natsConsumerConfig(s.bus.opts.Delivery, consumerName, s.channel, maxDeliver, ackWait))
	if err != nil {
		return fmt.Errorf("nats consumer %q: %w", consumerName, err)
	}

	// A dedicated DLQ stream receives poison messages past MaxDeliver so
	// they're never silently dropped (R-bus-3 — parity with the Redis
	// adapter's `<channel>.dlq`). Created before the advisory subscription
	// that feeds it.
	if err := s.bus.ensureDLQStream(ctx, s.channel); err != nil {
		return err
	}
	// The poison-outcome claim bucket both DLQ authorities (consume path +
	// advisory) arbitrate through (A-P9-1). Created eagerly so config/broker
	// failures surface here rather than inside the first advisory callback.
	if _, err := s.bus.ensureDLQClaims(ctx); err != nil {
		return err
	}

	handle := &natsConsumeHandle{}

	prefix := s.channel + "."
	bgCtx := context.WithoutCancel(ctx)
	cctx, err := consumer.Consume(func(msg jetstream.Msg) {
		// Same belt the Redis loops wear. The handler itself is
		// recover-wrapped, but everything AROUND it here — the observer
		// callbacks, the DLQ routing, the client plumbing — is not, and
		// nats.go carries no recover() of its own: a panic in any of it
		// takes the whole process down from a detached dispatch goroutine.
		defer recoverLoopTick(bgCtx, "eventbus nats consume")
		// Gate new work on the draining flag under the subscriber mutex so
		// the wg.Add below is atomic with the flag check — once Drain has
		// set draining (also under mu) no further Add fires, so it can't
		// race Drain's wg.Wait at the zero counter (R-bus-2). A message that
		// slips in mid-drain is Nak'd for redelivery, not silently acked.
		s.mu.Lock()
		if s.draining {
			s.mu.Unlock()
			_ = msg.Nak()
			return
		}
		s.wg.Add(1)
		s.mu.Unlock()
		defer s.wg.Done()

		// D1 — mark this stream sequence in-flight for the handler's
		// lifetime so a MAX_DELIVERIES advisory that fires mid-handler
		// (the slow-handler-past-AckWait race) defers the DLQ decision to
		// us instead of dropping a message that may still succeed. meta is
		// nil only for a non-JetStream message, which can't reach a
		// JetStream consumer — guard anyway and skip the bookkeeping.
		meta, metaErr := msg.Metadata()
		if metaErr == nil && meta != nil {
			release := s.markInFlight(consumerName, meta.Sequence.Stream)
			defer release()
		}

		// A-P9-1 — on the FINAL delivery attempt, claim this message's
		// terminal outcome BEFORE running the handler. The claim is
		// broker-shared, so it is visible to the advisory handler on ANY
		// replica (the in-flight mark above is not), and it closes the
		// prefetch window: if the MAX_DELIVERIES advisory already claimed
		// (and dead-lettered) this message while it sat past AckWait in
		// our client buffer, the delivery we are holding is a zombie —
		// running its handler would report a success for a message that
		// was just dropped, the incoherent drop+success this guard exists
		// to prevent. Skip the handler and Ack to settle the client side;
		// the drop was already surfaced by the advisory's OnDrop. A claim
		// ERROR proceeds as before the claim existed: at-least-once bias,
		// and the window is no worse than the pre-claim code. Below the
		// cap no claim is taken — the happy path pays no broker RTT.
		finalAttempt := meta != nil && metaErr == nil && maxDeliver > 0 && meta.NumDelivered >= uint64(maxDeliver)
		if finalAttempt {
			claimCtx, claimCancel := context.WithTimeout(bgCtx, 10*time.Second)
			owned, claimErr := s.claimPoisonOutcome(claimCtx, consumerName, meta.Sequence.Stream, natsDLQClaimRoleDelivery)
			claimCancel()
			if claimErr == nil && !owned {
				_ = msg.Ack()
				return
			}
		}

		topic := strings.TrimPrefix(msg.Subject(), prefix)
		start := time.Now()
		err := h(bgCtx, topic, msg.Data())
		reportDeliverLatency(s.bus.opts.Observer, s.channel, topic, time.Since(start))
		if err != nil {
			s.bus.opts.Observer.OnDeliverFailure(s.channel, topic, err)
			// D1 — at the delivery cap THIS path owns the DLQ (it claimed the
			// outcome above, or the claim errored and we fall back to the
			// pre-claim behaviour). Nak'ing here and waiting for the server's
			// MAX_DELIVERIES advisory is unsafe: while we were running past
			// AckWait the advisory already fired and either lost the claim or
			// was skipped (in-flight), so it won't act again (one-shot). DLQ
			// the poison message ourselves, then Ack so the server treats it
			// as done and no (duplicate) advisory fires. Below the cap, Nak
			// for normal redelivery.
			if finalAttempt {
				dlqCtx, dlqCancel := context.WithTimeout(bgCtx, 10*time.Second)
				ok := s.routeToDLQDirect(dlqCtx, topic, msg.Data())
				dlqCancel()
				if ok {
					_ = msg.Ack()
				} else {
					// DLQ write failed — leave it for redelivery / the
					// advisory fallback rather than acking it into the void.
					_ = msg.Nak()
				}
				return
			}
			// Nak triggers redelivery up to MaxDeliver per the consumer
			// config; once exhausted, the server fires a MAX_DELIVERIES
			// advisory the per-handle subscription below surfaces (the
			// fallback for a handler that never completes its last attempt).
			_ = msg.Nak()
			return
		}
		s.bus.opts.Observer.OnDeliverSuccess(s.channel, topic)
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("nats consume: %w", err)
	}
	handle.cctx = cctx

	// The MAX_DELIVERIES advisory subject is identical across every
	// replica running this consumer (it keys on the consumer name). A
	// plain core-NATS Subscribe delivers the advisory to ALL replicas,
	// so each one independently routes the poison message to the DLQ
	// and fires OnDrop → N× DLQ entries + an inflated drop metric. A
	// queue subscription scoped to the consumer name makes exactly one
	// replica handle each advisory. (In fan-out mode the consumer name
	// is already per-replica, so the group has one member and the
	// scoping is a no-op — correctly: there each replica has its OWN
	// delivery budget for its OWN copy of the message.)
	advSubject := natsMaxDeliverAdvisorySubject(s.channel, consumerName)
	advQueue := natsMaxDeliverAdvisoryQueue(consumerName)
	advSub, err := s.bus.conn.QueueSubscribe(advSubject, advQueue, func(m *natsgo.Msg) {
		s.handleAdvisory(bgCtx, consumerName, m.Data)
	})
	if err != nil {
		cctx.Stop()
		return fmt.Errorf("nats advisory subscribe %q: %w", advSubject, err)
	}
	handle.advisorySub = advSub

	// Register the handle atomically with the draining check. Drain snapshots
	// s.handles and stops only what it found, so an append landing after that
	// snapshot leaves a live consumer nothing will ever stop — and its
	// deliveries then vanish: the consume callback Naks them to the delivery
	// cap, and the MAX_DELIVERIES advisory that should dead-letter them is
	// gated on the same draining flag, so the poison message reaches neither
	// the handler nor the DLQ nor OnDrop. Losing the race means WE tear the
	// consumer down and report the refusal.
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		stopConsumeHandle(handle)
		return ErrSubscriberDraining
	}
	s.handles = append(s.handles, handle)
	s.mu.Unlock()
	return nil
}

// isDraining reports whether Drain has begun on this subscriber. The
// early-out it serves saves the consumer/stream round-trips; the
// load-bearing check is the one taken atomically with the handle
// registration at the end of Subscribe.
func (s *natsSubscriber) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// stopConsumeHandle tears one handle down: advisory subscription first (so no
// further advisory work is queued), then the consume loop. Shared by Drain and
// by the late-Subscribe refusal, which must leave nothing running.
func stopConsumeHandle(h *natsConsumeHandle) {
	if h.advisorySub != nil {
		_ = h.advisorySub.Unsubscribe()
	}
	if h.cctx != nil {
		h.cctx.Stop()
	}
}

// natsMaxDeliverAdvisorySubject composes the JetStream
// advisory subject the server publishes on whenever a
// message exceeds the consumer's MaxDeliver setting. Format
// `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer>`
// matches the server constant `JSAdvisoryConsumerMaxDeliveryExceedPre`.
func natsMaxDeliverAdvisorySubject(stream, consumer string) string {
	return "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + stream + "." + consumer
}

// natsMaxDeliverAdvisoryQueue is the queue-group name the per-handle
// advisory subscription joins. Scoping it to the consumer's durable
// name means every replica that runs this durable shares one queue
// group, so the server load-balances each MAX_DELIVERIES advisory to
// exactly one replica — preventing duplicate DLQ routing + a multiplied
// OnDrop metric in multi-replica deployments. The `_w17adv-` prefix
// keeps it out of any author-defined queue namespace.
func natsMaxDeliverAdvisoryQueue(consumer string) string {
	return "_w17adv-" + consumer
}

// maxDeliverAdvisory is the subset of the JetStream
// `consumer_max_delivery_exceed` advisory payload routeToDLQ
// needs: the source stream + the sequence of the message that
// exhausted its delivery attempts.
type maxDeliverAdvisory struct {
	Stream    string `json:"stream"`
	StreamSeq uint64 `json:"stream_seq"`
}

// routeToDLQ forwards a poison message (one that exhausted
// MaxDeliver, surfaced by the server's MAX_DELIVERIES advisory)
// to the channel's dead-letter stream, then fires Observer.OnDrop
// — DLQ parity with the Redis adapter (R-bus-3). It fetches the
// original payload from the main stream by the advisory's
// stream_seq and republishes it to the `_dlq.<channel>[.<topic>]`
// subject, preserving the dispatch-time topic.
//
// OnDrop fires ONLY after the payload is safely in the DLQ; if the
// advisory can't be parsed or the source/republish round-trip
// fails, it surfaces OnDeliverFailure instead so a drop is never
// reported for a message that didn't actually reach the DLQ.
//
// Q61-bus-1: the MAX_DELIVERIES advisory rides core NATS, which is
// fire-and-forget — there is no redelivery, so a transient broker
// hiccup during the DLQ round-trip would otherwise permanently skip
// the dead letter. Each broker op (source fetch + republish) is
// therefore wrapped in a bounded retry so a momentary failure is
// ridden out rather than dropping the poison message from the DLQ.
// handleAdvisory is the MAX_DELIVERIES advisory callback. It mirrors the
// consume callback's drain coordination (R-bus-2): gate new DLQ-routing work
// on the draining flag under the subscriber mutex so the wg.Add is atomic with
// the flag check. Without this an advisory that arrives mid-Drain would run
// routeToDLQ after Drain's wg.Wait returned — operating on a connection
// Close() is about to tear down (B4-eventbus-2).
func (s *natsSubscriber) handleAdvisory(ctx context.Context, consumer string, payload []byte) {
	// The advisory arrives on core NATS's async dispatcher, which likewise
	// has no recovery of its own.
	defer recoverLoopTick(ctx, "eventbus nats advisory")
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	s.routeToDLQ(ctx, consumer, payload)
}

func (s *natsSubscriber) routeToDLQ(ctx context.Context, consumer string, advisoryPayload []byte) {
	var adv maxDeliverAdvisory
	if err := json.Unmarshal(advisoryPayload, &adv); err != nil || adv.StreamSeq == 0 {
		// Can't locate the source message — report a deliver failure
		// rather than a DLQ drop we couldn't actually perform.
		s.bus.opts.Observer.OnDeliverFailure(s.channel, "", errAdvisoryUnparsable(err))
		return
	}

	// D1 — a handler in THIS process is still processing this sequence (it
	// overran AckWait on its last delivery, which is why the advisory
	// fired). Defer the DLQ decision to the consume path: on success it
	// Acks (no DLQ); on failure at the cap it DLQs itself. Routing here
	// would drop + OnDrop a message that may yet succeed. Mirrors the Redis
	// in-flight guard (R-bus-1). This is only the local fast path — the
	// load-bearing, replica-agnostic guard is the claim below (A-P9-1).
	if s.inFlightBusy(consumer, adv.StreamSeq) {
		return
	}

	// Bound the DLQ round-trip so a hung broker during shutdown can't
	// wedge the advisory callback forever.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// A-P9-1 — claim the poison outcome before dead-lettering. The
	// process-local map above cannot see a handler mid-run on ANOTHER
	// replica (the advisory queue group is cluster-wide), nor a delivery
	// parked past AckWait in a client prefetch buffer that a handler will
	// still pick up; and the broker keeps no re-checkable terminal state
	// (at advisory time it has already ack-floored past the message, and a
	// late ack leaves no per-message trace — see claimPoisonOutcome). If
	// the claim is LOST, a delivery path owns/owned this message's outcome
	// (its handler decides: Ack on success, DLQ on failure at the cap) —
	// dead-lettering here would both double-report drops and drop messages
	// that succeed. If the claim ERRORS, surface a deliver failure and do
	// NOT dead-letter: acting without the claim reopens the drop+success
	// incoherence, and any message a live client still holds reaches the
	// DLQ through its delivery path anyway.
	owned, claimErr := s.claimPoisonOutcome(ctx, consumer, adv.StreamSeq, natsDLQClaimRoleAdvisory)
	if claimErr != nil {
		s.bus.opts.Observer.OnDeliverFailure(s.channel, "", claimErr)
		return
	}
	if !owned {
		return
	}

	var stream jetstream.Stream
	if err := retryBrokerOp(ctx, natsDLQRetryAttempts, natsDLQRetryBackoff, func() error {
		var e error
		stream, e = s.bus.js.Stream(ctx, adv.Stream)
		return e
	}); err != nil {
		s.bus.opts.Observer.OnDeliverFailure(s.channel, "", err)
		return
	}
	var raw *jetstream.RawStreamMsg
	if err := retryBrokerOp(ctx, natsDLQRetryAttempts, natsDLQRetryBackoff, func() error {
		var e error
		raw, e = stream.GetMsg(ctx, adv.StreamSeq)
		return e
	}); err != nil {
		s.bus.opts.Observer.OnDeliverFailure(s.channel, "", err)
		return
	}

	topic := strings.TrimPrefix(raw.Subject, s.channel+".")
	if topic == raw.Subject {
		// No `<channel>.` prefix (topic-less emit) — DLQ under the bare
		// channel subject.
		topic = ""
	}
	s.routeToDLQDirect(ctx, topic, raw.Data)
}

// routeToDLQDirect publishes a poison message's payload to the channel's
// dead-letter subject and fires Observer.OnDrop on success, or
// OnDeliverFailure if the DLQ write can't be performed. It is the shared
// DLQ-write step for both DLQ authorities: the advisory callback
// (routeToDLQ, after fetching the payload by stream_seq) and the consume
// path (D1, which already holds the payload when a handler fails at the
// delivery cap). Returns true iff the payload reached the DLQ. The caller
// supplies its own context deadline (the consume path inherits the handler
// ctx; routeToDLQ wraps a 10s shutdown bound).
func (s *natsSubscriber) routeToDLQDirect(ctx context.Context, topic string, data []byte) bool {
	if err := retryBrokerOp(ctx, natsDLQRetryAttempts, natsDLQRetryBackoff, func() error {
		_, e := s.bus.js.Publish(ctx, natsDLQSubject(s.channel, topic), data)
		return e
	}); err != nil {
		// Couldn't move it to the DLQ — surface a failure, not a drop.
		s.bus.opts.Observer.OnDeliverFailure(s.channel, topic, err)
		return false
	}
	s.bus.opts.Observer.OnDrop(s.channel, topic, DropReasonMaxDeliverExceeded)
	return true
}

// natsDLQRetryAttempts / natsDLQRetryBackoff bound the retry loop that
// rides out a transient broker failure during DLQ routing (Q61-bus-1).
// Three attempts with a short fixed backoff stay well inside the 10s
// round-trip deadline while covering a momentary broker hiccup.
const (
	natsDLQRetryAttempts = 3
	natsDLQRetryBackoff  = 200 * time.Millisecond
)

// retryBrokerOp runs fn up to attempts times, returning nil on the first
// success and the last error once every attempt fails. A fixed backoff
// separates attempts; ctx cancellation aborts the wait and returns
// ctx.Err() immediately so a cancelled/expired round-trip doesn't keep
// retrying. attempts <= 1 runs fn exactly once with no backoff.
func retryBrokerOp(ctx context.Context, attempts int, backoff time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

// errAdvisoryUnparsable wraps the advisory-decode failure (or
// synthesises one when the payload parsed but carried no usable
// stream_seq) for the OnDeliverFailure surface.
func errAdvisoryUnparsable(err error) error {
	if err != nil {
		return fmt.Errorf("nats dlq: parse max-deliver advisory: %w", err)
	}
	return errors.New("nats dlq: max-deliver advisory missing stream_seq")
}

// natsDLQStreamName is the dead-letter stream backing `channel`.
// NATS stream names forbid dots, so the Redis `<channel>.dlq`
// convention becomes `<channel>_dlq` here.
func natsDLQStreamName(channel string) string {
	return channel + "_dlq"
}

// natsDLQSubjects returns the (base, wildcard) subject pair the DLQ
// stream captures: `_dlq.<channel>` for topic-less emits and
// `_dlq.<channel>.>` for everything carrying a topic. The `_dlq.`
// prefix keeps the DLQ outside the main stream's `<channel>.>`
// filter so DLQ'd messages aren't re-consumed.
func natsDLQSubjects(channel string) (base, wildcard string) {
	return "_dlq." + channel, "_dlq." + channel + ".>"
}

// natsDLQSubject composes the concrete DLQ subject for one
// dropped message, preserving its topic suffix when present.
func natsDLQSubject(channel, topic string) string {
	if topic == "" {
		return "_dlq." + channel
	}
	return "_dlq." + channel + "." + topic
}

// Drain stops every consume loop + advisory subscription and
// waits for in-flight handlers via the bus-shared WaitGroup,
// bounded by `ctx`.
func (s *natsSubscriber) Drain(ctx context.Context) error {
	s.mu.Lock()
	// Set draining BEFORE stopping the handles so any callback that fires
	// after this point short-circuits without a wg.Add (R-bus-2).
	s.draining = true
	handles := append([]*natsConsumeHandle(nil), s.handles...)
	s.handles = nil
	s.mu.Unlock()

	for _, h := range handles {
		stopConsumeHandle(h)
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

// natsSubject composes the per-emit NATS subject from channel
// + topic. Tokens are dot-separated; empty topic collapses to
// just the channel name (legal but unusual — every emit
// should carry a topic).
func natsSubject(channel, topic string) string {
	if topic == "" {
		return channel
	}
	return channel + "." + topic
}

// sanitizeDurableSuffix turns a topic-glob filter into a
// JetStream-safe durable-name suffix. NATS durable names
// allow alphanumeric + `-` + `_`; dots and wildcards (`*`,
// `**`) get translated.
func sanitizeDurableSuffix(filter string) string {
	var b strings.Builder
	b.Grow(len(filter))
	for _, r := range filter {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.':
			b.WriteByte('_')
		case r == '*':
			b.WriteByte('s')
		default:
			b.WriteByte('x')
		}
	}
	return b.String()
}
