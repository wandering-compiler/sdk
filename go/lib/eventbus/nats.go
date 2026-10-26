package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/dsnredact"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// flows into every consumer's MaxDeliver + AckWait;
// ChannelRetries narrows it per channel and per topic.
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
	// DefaultObserver (failures and drops traced, successes silent);
	// NopObserver is the explicit silent opt-out.
	Observer Observer

	// ChannelRetries overrides DefaultMaxDeliver +
	// DefaultAckWait per-channel. Channel name -> retry
	// override. Channels missing from the map use the
	// Default* values. Codegen-generated wiring fills this from
	// the parser's per-event ResolvedRetry rolled up to
	// channel granularity. Keys must be valid channel names
	// ([ValidateChannel]); NewNatsBus refuses one that is not.
	ChannelRetries map[string]ChannelRetry

	// Retention bounds every channel stream this bus creates (JetStream
	// MaxAge / MaxMsgs, oldest discarded first). Zero value -> messages
	// older than [DefaultStreamMaxAge] are removed, no count limit. The
	// limits are (re)applied each time the bus ensures the stream, so they
	// replace limits configured on the broker by other means.
	Retention StreamRetention

	// DLQRetention bounds every dead-letter stream this bus creates. Zero
	// value -> [DefaultDLQMaxAge], no count limit.
	DLQRetention StreamRetention

	// PublishTimeout bounds one Dispatch: the stream check, the publish and
	// the wait for JetStream's acknowledgement. Zero ->
	// [DefaultPublishTimeout]. A Dispatch that runs out of time fails (and
	// is reported to the Observer) instead of holding its caller for the
	// client's full request timeout during a broker outage.
	PublishTimeout time.Duration

	// ConnectWait is how long NewNatsBus keeps retrying a broker it cannot
	// REACH (name not resolvable yet, connection refused) before it gives
	// up. Zero -> [DefaultConnectWait]; negative -> one attempt. A broker
	// that answers and refuses (bad token) fails at once whatever this is.
	ConnectWait time.Duration
}

// DefaultConnectWait is [NatsBusOptions.ConnectWait] when unset.
//
// Long enough for a broker started in the same deploy: Swarm has no
// depends_on, and on 2026-10-22 the console's first deploy with console-nats
// started console-app about a second before the new service's name resolved.
// The process exited, Swarm read that as a failed update and rolled the whole
// console back.
const DefaultConnectWait = 30 * time.Second

// NatsBus implements Dispatcher + SubscriberFactory against a
// NATS JetStream cluster.
//
// Channel name → JetStream stream name (lazy-created with a
// `<channel>.>` subject filter on first use). Topic → subject
// suffix (emit on channel `default` with topic `user.created`
// publishes to subject `default.user.created`). Subscriber
// uses subject filter `<channel>.>` (catch-all) and applies the
// subscription's topic filter with MatchGlob before invoking the
// handler. The generated subscribers register `**`, so they run
// one consumer per channel and filter per event in dispatch.
//
// Retry / DLQ are driven by JetStream consumer config:
// `MaxDeliver` from NatsBusOptions.DefaultMaxDeliver,
// `AckWait` from .DefaultAckWait. When a message exhausts
// MaxDeliver the server fires a MAX_DELIVERIES advisory; the
// per-handle advisory subscription forwards the original payload
// to the channel's dead-letter stream (`<channel>_dlq`, subjects
// `_dlq.<channel>[.<topic>]`) and fires Observer.OnDrop — parity
// with the Redis adapter's `<channel>.dlq` routing.
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
	// incoherent drop+success the in-flight guard closed, through the guard's blind spot. The
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
		return nil, fmt.Errorf("NatsBusOptions: DSN %q must use nats:// or tls:// scheme", dsnredact.String(opts.DSN))
	}
	// The prefix starts every JetStream consumer name, which forbids `.`,
	// `*`, `>` and whitespace: refuse it here rather than at the first
	// Subscribe.
	for _, r := range opts.DurablePrefix {
		if !isNameRune(r) {
			return nil, fmt.Errorf("NatsBusOptions: DurablePrefix %q contains %q (allowed: letters, digits, '_' and '-')", opts.DurablePrefix, r)
		}
	}
	if err := validateChannelKeys("NatsBusOptions.ChannelRetries", opts.ChannelRetries); err != nil {
		return nil, err
	}
	if opts.PublishTimeout <= 0 {
		opts.PublishTimeout = DefaultPublishTimeout
	}
	if opts.DefaultMaxDeliver == 0 {
		opts.DefaultMaxDeliver = 3
	}
	if opts.DefaultAckWait == 0 {
		opts.DefaultAckWait = 30 * time.Second
	}
	if opts.ConnectWait == 0 {
		opts.ConnectWait = DefaultConnectWait
	}
	// Nil -> DefaultObserver; every callback made panic-safe.
	opts.Observer = guardObserver(opts.Observer)
	if opts.InstanceID == "" {
		opts.InstanceID = newInstanceID()
	} else {
		opts.InstanceID = sanitizeInstanceID(opts.InstanceID)
	}

	// A LINK THAT DROPS HAS TO BE AUDIBLE.
	//
	// natsgo.Connect with no options reconnects on its own — and says nothing,
	// at any point, whatever happens. When an ordinary `docker compose up -d`
	// recreates the broker, delivery stops and the subscriber's log holds one
	// line from before the restart and nothing after it: a sign-in returns 200,
	// its challenge row appears, and the code never arrives.
	//
	// These four handlers do not repair anything. They make the difference
	// between "the bus is quiet because nothing was emitted" and "the bus is
	// quiet because it is not connected" visible from a log, which is the only
	// place an operator can look.
	natsOpts := []natsgo.Option{
		// A close THIS process asks for (NatsBus.Close at shutdown) fires no
		// handler at all; one with any other cause still fires them all.
		//
		// Without it, nats.go ran the disconnect and closed handlers below on
		// every ordinary shutdown — SIGTERM on a retired blue/green colour —
		// and each one put "connection closed permanently" into Sentry and
		// "connection LOST" into the log: one false
		// error per eventbus bundle per deploy, burying the real outage.
		//
		// The decision is made where the close is CAUSED, not where a callback
		// later runs: a flag read inside the handler would also swallow a real
		// outage whose callback was already queued when Close set it.
		natsgo.NoCallbacksAfterClientClose(),
		natsgo.DisconnectErrHandler(func(_ *natsgo.Conn, cerr error) {
			log.Printf("eventbus nats: connection LOST (%v) — delivery is stopped until it returns", cerr)
		}),
		natsgo.ReconnectHandler(func(c *natsgo.Conn) {
			// Reconnected is not the same as subscribed. If the server was
			// recreated rather than restarted, its JetStream state went with
			// it: the streams and consumers this process was using are gone,
			// and the consume loop below is what reports that.
			log.Printf("eventbus nats: reconnected to %s — verifying consumers", dsnredact.String(c.ConnectedUrl()))
		}),
		natsgo.ClosedHandler(func(_ *natsgo.Conn) {
			// Terminal. nats.go gives up after MaxReconnects and never tries
			// again, so a process that reaches here delivers nothing for the
			// rest of its life and must be restarted. (Never reached by our own
			// Close — see NoCallbacksAfterClientClose above.)
			log.Print("eventbus nats: connection CLOSED for good — this process will deliver nothing until it is restarted")
			observx.ReportError(context.Background(), errors.New("eventbus nats: connection closed permanently"))
		}),
		natsgo.ErrorHandler(func(_ *natsgo.Conn, sub *natsgo.Subscription, aerr error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			log.Printf("eventbus nats: async error on %q: %v", subject, aerr)
		}),
	}
	nc, err := connectNats(opts.DSN, opts.ConnectWait, natsOpts)
	if err != nil {
		// The DSN carries the broker's credential (a bare userinfo is the
		// whole token), and this error goes to the log.
		return nil, fmt.Errorf("nats connect %s: %w", dsnredact.String(opts.DSN), err)
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

// connectNats dials the broker, retrying for up to wait while it cannot be
// reached at all. Only a network error is retried: a broker that answered and
// refused (authorization, a bad DSN) gives the same answer a second later.
func connectNats(dsn string, wait time.Duration, natsOpts []natsgo.Option) (*natsgo.Conn, error) {
	deadline := time.Now().Add(wait)
	logged := false
	for {
		nc, err := natsgo.Connect(dsn, natsOpts...)
		if err == nil || !brokerUnreachable(err) || !time.Now().Before(deadline) {
			return nc, err
		}
		if !logged {
			log.Printf("eventbus nats: broker %s not reachable yet (%v) — retrying for up to %s",
				dsnredact.String(dsn), err, wait)
			logged = true
		}
		time.Sleep(connectRetryInterval)
	}
}

// connectRetryInterval spaces the attempts of [connectNats].
const connectRetryInterval = 500 * time.Millisecond

// brokerUnreachable reports whether a connect error means no broker answered.
func brokerUnreachable(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, natsgo.ErrNoServers)
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
		// Said here, by the closer: the connection's own handlers stay silent
		// for a close this process asked for (NoCallbacksAfterClientClose).
		log.Print("eventbus nats: connection closed (shutdown)")
	}
	return nil
}

// Dispatch publishes one envelope to JetStream on the
// `<channel>.<topic>` subject. Returns the publish error
// directly; the generated emit interceptor wraps the call in
// `_ = bus.Dispatch(...)` per fire-and-forget semantics.
func (b *NatsBus) Dispatch(ctx context.Context, channel, topic string, envelope proto.Message) error {
	if err := validateEmit(channel, topic); err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	if b.conn.IsClosed() {
		// The client gave up reconnecting (or Close ran): no publish can
		// succeed, so do not spend the timeout finding that out per event.
		err := fmt.Errorf("eventbus nats: connection is closed: %w", natsgo.ErrConnectionClosed)
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	// Detach from the caller's (cancellable) RPC ctx so a fire-and-forget
	// emit isn't lost when the originating request ctx is already cancelled
	// — mirrors MemoryBus.Dispatch. Trace/baggage carry forward;
	// cancellation does not. The emit runs on the committing goroutine, so
	// the whole of it — stream check, publish, acknowledgement — is bounded
	// by PublishTimeout: during an outage each event fails after that long
	// instead of after the client's own request timeout.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.opts.PublishTimeout)
	defer cancel()
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
	_, err = b.js.Publish(pubCtx, subject, raw)
	if err != nil && natsStreamMissing(err) {
		// The stream cache says the stream exists, but no stream answered:
		// the broker lost it (recreated without persistent storage, or the
		// stream was deleted). Without this every later emit on the channel
		// failed until the process restarted. Forget the cached entry,
		// re-create the stream and publish once more.
		b.forgetStream(channel)
		if err = b.ensureStream(pubCtx, channel); err == nil {
			_, err = b.js.Publish(pubCtx, subject, raw)
		}
	}
	if err != nil {
		b.opts.Observer.OnEmitFailure(channel, topic, err)
		return err
	}
	b.opts.Observer.OnEmitSuccess(channel, topic)
	return nil
}

// natsStreamMissing reports whether a publish failed because no stream
// captured the subject.
func natsStreamMissing(err error) bool {
	return errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, natsgo.ErrNoResponders)
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
	if err := ValidateChannel(channel); err != nil {
		return nil, err
	}
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
	applyNatsRetention(&cfg, b.opts.Retention, DefaultStreamMaxAge)
	_, err := b.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ensure stream %q: %w", channel, err)
	}

	b.mu.Lock()
	b.streams[channel] = true
	b.mu.Unlock()
	return nil
}

// applyNatsRetention writes the resolved retention limits onto a stream
// config. Oldest messages are discarded first; -1 is JetStream's "no limit".
func applyNatsRetention(cfg *jetstream.StreamConfig, r StreamRetention, defaultAge time.Duration) {
	maxAge, maxMsgs := r.limits(defaultAge)
	cfg.Discard = jetstream.DiscardOld
	cfg.MaxAge = maxAge
	cfg.MaxMsgs = -1
	if maxMsgs > 0 {
		cfg.MaxMsgs = maxMsgs
	}
}

// ensureDLQStream materialises the dead-letter stream that
// receives messages a consumer drops past MaxDeliver.
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
	applyNatsRetention(&cfg, b.opts.DLQRetention, DefaultDLQMaxAge)
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
	// second subscriber kept delivering during the first's drain.
	wg sync.WaitGroup
	// draining is set (under mu) when Drain begins, BEFORE the consume
	// handles are Stop()'d. The Consume callback checks it (also under
	// mu) and short-circuits without a wg.Add once draining — so THIS
	// subscriber's per-message Add can't race its own Drain's wg.Wait
	// across the zero counter (the WaitGroup-reuse panic).
	draining bool
}

// markInFlight records a stream sequence as being processed under (channel,
// durable) and returns the release func (parity with the Redis adapter's
// in-flight guard). The MAX_DELIVERIES advisory can fire while a slow handler on
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
// is recovered and matched against topicFilter ([MatchGlob]);
// only a match is passed to the handler; on handler success the
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
	h = deliveryHandler(h)
	consumerName := s.bus.opts.DurablePrefix + "-" + s.channel
	if topicFilter != "" && topicFilter != "**" {
		consumerName += "-" + sanitizeDurableSuffix(topicFilter)
	}
	if s.bus.opts.Delivery == DeliveryFanOut {
		consumerName += "-" + s.bus.opts.InstanceID
	}

	maxDeliver, ackWait := resolveChannelRetry(s.bus.opts.ChannelRetries, s.channel, s.bus.opts.DefaultMaxDeliver, s.bus.opts.DefaultAckWait)

	consumerCfg := natsConsumerConfig(s.bus.opts.Delivery, consumerName, s.channel, maxDeliver, ackWait)

	// A dedicated DLQ stream receives poison messages past MaxDeliver so
	// they're never silently dropped (parity with the Redis adapter's
	// `<channel>.dlq`). Created before the advisory subscription
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
	onMsg := func(msg jetstream.Msg) {
		// Same belt the Redis loops wear. The handler itself is
		// recover-wrapped, but everything AROUND it here — the observer
		// callbacks, the DLQ routing, the client plumbing — is not, and
		// nats.go carries no recover() of its own: a panic in any of it
		// takes the whole process down from a detached dispatch goroutine.
		defer recoverLoopTick(bgCtx, "eventbus nats consume")
		// Gate new work on the draining flag under the subscriber mutex so
		// the wg.Add below is atomic with the flag check — once Drain has
		// set draining (also under mu) no further Add fires, so it can't
		// race Drain's wg.Wait at the zero counter. A message that
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

		// The consumer reads the whole channel (`<channel>.>`), so the
		// subscription's own topic filter is applied here, with the same
		// MatchGlob the memory bus uses: a handler subscribed to `user.*`
		// must not be handed `order.created`. The consumer is per filter
		// (see consumerName), so a message outside it is simply settled —
		// another subscription's consumer has its own copy.
		topic := strings.TrimPrefix(msg.Subject(), prefix)
		if !MatchGlob(topicFilter, topic) {
			_ = msg.Ack()
			return
		}

		// Mark this stream sequence in-flight for the handler's
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
		// The topic's own retry, when its channel has one: a lower cap
		// than the consumer's, and the delay before the next attempt.
		// The consumer stops at the channel's MaxDeliver; a topic that
		// asks for fewer reaches its final attempt here, below that cap,
		// and this path dead-letters it as at the cap.
		plan := resolveTopicRetry(s.bus.opts.ChannelRetries, s.channel, topic, s.bus.opts.DefaultMaxDeliver, s.bus.opts.DefaultAckWait)
		finalAttempt := meta != nil && metaErr == nil && plan.maxDeliver > 0 && meta.NumDelivered >= uint64(plan.maxDeliver)
		if finalAttempt {
			claimCtx, claimCancel := context.WithTimeout(bgCtx, 10*time.Second)
			owned, claimErr := s.claimPoisonOutcome(claimCtx, consumerName, meta.Sequence.Stream, natsDLQClaimRoleDelivery)
			claimCancel()
			if claimErr == nil && !owned {
				_ = msg.Ack()
				return
			}
		}

		start := time.Now()
		err := h(bgCtx, topic, msg.Data())
		reportDeliverLatency(s.bus.opts.Observer, s.channel, topic, time.Since(start))
		if err != nil {
			s.bus.opts.Observer.OnDeliverFailure(s.channel, topic, err)
			// At the delivery cap THIS path owns the DLQ (it claimed the
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
			// With a backoff the redelivery waits for it: NakWithDelay, as
			// a plain Nak is redelivered at once.
			attempt := 1
			if meta != nil && metaErr == nil {
				attempt = int(meta.NumDelivered)
			}
			if d := plan.delay(attempt); d > 0 {
				_ = msg.NakWithDelay(d)
				return
			}
			_ = msg.Nak()
			return
		}
		s.bus.opts.Observer.OnDeliverSuccess(s.channel, topic)
		_ = msg.Ack()
	}

	// startConsume creates (or re-creates) the consumer and starts pulling
	// from it. Used once here and again by recoverConsumer when the broker
	// has lost the consumer.
	var startConsume func(context.Context) (jetstream.ConsumeContext, error)
	var recovering atomic.Bool
	// THE CONSUME LOOP'S OWN FAILURES, which are otherwise discarded.
	//
	// Consume retries internally and reports what it hit here. With no
	// handler the errors go nowhere, and the loudest case is a broker that
	// was RECREATED, not restarted, so its JetStream state went with it: the
	// client reconnects to a server that has never heard of this consumer,
	// every pull fails, and delivery simply stops.
	//
	// Reported, and — when the error says the consumer may be gone — handed
	// to recoverConsumer, which re-creates the stream and the consumer and
	// restarts the pull loop. One recovery at a time per subscription.
	errHandler := jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, cerr error) {
		reportBusFailure("consume", s.channel, consumerName, cerr)
		if !natsConsumerMaybeLost(cerr) || !recovering.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer recovering.Store(false)
			s.recoverConsumer(bgCtx, handle, consumerName, errors.Is(cerr, jetstream.ErrConsumerDeleted), startConsume)
		}()
	})
	startConsume = func(ctx context.Context) (jetstream.ConsumeContext, error) {
		consumer, err := s.bus.js.CreateOrUpdateConsumer(ctx, s.channel, consumerCfg)
		if err != nil {
			return nil, fmt.Errorf("nats consumer %q: %w", consumerName, err)
		}
		cctx, err := consumer.Consume(onMsg, errHandler)
		if err != nil {
			return nil, fmt.Errorf("nats consume: %w", err)
		}
		return cctx, nil
	}
	cctx, err := startConsume(ctx)
	if err != nil {
		return err
	}

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

	// Register the handle atomically with the draining check. Drain snapshots
	// s.handles and stops only what it found, so an append landing after that
	// snapshot leaves a live consumer nothing will ever stop — and its
	// deliveries then vanish: the consume callback Naks them to the delivery
	// cap, and the MAX_DELIVERIES advisory that should dead-letter them is
	// gated on the same draining flag, so the poison message reaches neither
	// the handler nor the DLQ nor OnDrop. Losing the race means WE tear the
	// consumer down and report the refusal.
	//
	// The handle's fields are written under the same lock: recoverConsumer
	// swaps cctx under it, and only on a registered handle.
	s.mu.Lock()
	handle.cctx = cctx
	handle.advisorySub = advSub
	if s.draining {
		s.mu.Unlock()
		stopConsumeHandle(handle)
		return ErrSubscriberDraining
	}
	s.handles = append(s.handles, handle)
	s.mu.Unlock()
	return nil
}

// natsConsumerMaybeLost reports whether a consume-loop error can mean the
// broker no longer has this consumer: deleted with its stream, unknown to
// a recreated server (pulls then find no responder), or silent long enough
// to miss heartbeats. A transient cause is told apart by recoverConsumer,
// which checks the consumer before re-creating anything.
func natsConsumerMaybeLost(err error) bool {
	return errors.Is(err, jetstream.ErrConsumerDeleted) ||
		errors.Is(err, jetstream.ErrConsumerNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrNoHeartbeat) ||
		errors.Is(err, natsgo.ErrNoResponders)
}

// natsRecoverBackoff spaces the attempts to re-create a lost consumer while
// the broker is still unreachable.
const natsRecoverBackoff = time.Second

// recoverConsumer re-establishes a subscription whose consumer the broker
// lost — a server recreated without persistent storage, or the stream
// deleted under a live process. Without it the consume loop stopped (or
// pulled from nothing) for the rest of the process's life.
//
// Unless the loop already reported the consumer deleted (which also stops
// the loop), the consumer is looked up first: if it still exists the error
// was transient and nothing is touched. Otherwise the cached stream entries
// are dropped, the stream, dead-letter stream and claim bucket re-ensured,
// and the consumer re-created with a fresh pull loop swapped into the
// handle — retried until it succeeds or the subscriber drains.
func (s *natsSubscriber) recoverConsumer(ctx context.Context, handle *natsConsumeHandle, consumerName string, stopped bool,
	start func(context.Context) (jetstream.ConsumeContext, error)) {
	for attempt := 0; ; attempt++ {
		if s.isDraining() || s.bus.isClosed() {
			return
		}
		if attempt > 0 {
			sleepCtx(ctx, natsRecoverBackoff)
		}
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if !stopped {
			_, err := s.bus.js.Consumer(opCtx, s.channel, consumerName)
			if err == nil {
				cancel()
				return
			}
			if !errors.Is(err, jetstream.ErrConsumerNotFound) && !errors.Is(err, jetstream.ErrStreamNotFound) {
				// Cannot tell (broker unreachable): the loop reports again
				// and a later error retries the check.
				cancel()
				return
			}
		}
		cctx, err := s.bus.reensureChannel(opCtx, s.channel, start)
		cancel()
		if err != nil {
			reportBusFailure("consumer recreate", s.channel, consumerName, err)
			continue
		}
		s.mu.Lock()
		if s.draining || handle.cctx == nil {
			// Drain has begun (or the handle was never registered): the new
			// loop belongs to nobody, so stop it here.
			s.mu.Unlock()
			cctx.Stop()
			return
		}
		old := handle.cctx
		handle.cctx = cctx
		s.mu.Unlock()
		old.Stop()
		log.Printf("eventbus nats: consumer %q on channel %q re-created after the broker lost it", consumerName, s.channel)
		return
	}
}

// reensureChannel drops the cached state for channel's streams and claim
// bucket, re-creates them on the broker, and runs start to re-create the
// consumer.
func (b *NatsBus) reensureChannel(ctx context.Context, channel string,
	start func(context.Context) (jetstream.ConsumeContext, error)) (jetstream.ConsumeContext, error) {
	b.forgetStream(channel)
	b.forgetStream(natsDLQStreamName(channel))
	b.mu.Lock()
	b.claims = nil
	b.mu.Unlock()
	if err := b.ensureStream(ctx, channel); err != nil {
		return nil, err
	}
	if err := b.ensureDLQStream(ctx, channel); err != nil {
		return nil, err
	}
	if _, err := b.ensureDLQClaims(ctx); err != nil {
		return nil, err
	}
	return start(ctx)
}

// forgetStream drops a stream from the bus's "already created" cache, so the
// next ensureStream re-issues the create against the broker.
func (b *NatsBus) forgetStream(name string) {
	b.mu.Lock()
	delete(b.streams, name)
	b.mu.Unlock()
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
// drainConsumeHandle is stopConsumeHandle for a subscriber going away:
// prefetched messages still reach the consume callback (see Drain).
func drainConsumeHandle(h *natsConsumeHandle) {
	if h.advisorySub != nil {
		_ = h.advisorySub.Unsubscribe()
	}
	if h.cctx != nil {
		h.cctx.Drain()
	}
}

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
// — DLQ parity with the Redis adapter. It fetches the
// original payload from the main stream by the advisory's
// stream_seq and republishes it to the `_dlq.<channel>[.<topic>]`
// subject, preserving the dispatch-time topic.
//
// OnDrop fires ONLY after the payload is safely in the DLQ; if the
// advisory can't be parsed or the source/republish round-trip
// fails, it surfaces OnDeliverFailure instead so a drop is never
// reported for a message that didn't actually reach the DLQ.
//
// The MAX_DELIVERIES advisory rides core NATS, which is
// fire-and-forget — there is no redelivery, so a transient broker
// hiccup during the DLQ round-trip would otherwise permanently skip
// the dead letter. Each broker op (source fetch + republish) is
// therefore wrapped in a bounded retry so a momentary failure is
// ridden out rather than dropping the poison message from the DLQ.
// handleAdvisory is the MAX_DELIVERIES advisory callback. It mirrors the
// consume callback's drain coordination: gate new DLQ-routing work
// on the draining flag under the subscriber mutex so the wg.Add is atomic with
// the flag check. Without this an advisory that arrives mid-Drain would run
// routeToDLQ after Drain's wg.Wait returned — operating on a connection
// Close() is about to tear down.
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

	// A handler in THIS process is still processing this sequence (it
	// overran AckWait on its last delivery, which is why the advisory
	// fired). Defer the DLQ decision to the consume path: on success it
	// Acks (no DLQ); on failure at the cap it DLQs itself. Routing here
	// would drop + OnDrop a message that may yet succeed. Mirrors the Redis
	// in-flight guard. This is only the local fast path — the
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
// path (which already holds the payload when a handler fails at the
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
// rides out a transient broker failure during DLQ routing.
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
	// after this point short-circuits without a wg.Add.
	s.draining = true
	handles := append([]*natsConsumeHandle(nil), s.handles...)
	s.handles = nil
	s.mu.Unlock()

	// Drain, not Stop: Stop discards the messages the client has already
	// prefetched, and the broker redelivers those only when AckWait runs
	// out — every rolling restart parked them for up to 30s. Drain runs
	// them through the callback, which Naks them (draining is set) so
	// another replica gets them at once; Drain then waits until the buffer
	// is empty, so none is left for Close to discard.
	for _, h := range handles {
		drainConsumeHandle(h)
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		for _, h := range handles {
			if h.cctx != nil {
				<-h.cctx.Closed()
			}
		}
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

// sanitizeDurableSuffix turns a topic-glob filter into a consumer-name
// suffix that is legal on both brokers (JetStream consumer names allow
// letters, digits, `_` and `-`) and INJECTIVE: two distinct filters never
// share a suffix, so two subscriptions on one channel never share — and
// compete for — one consumer.
//
// Letters and digits are kept and `.` becomes `_`, so a plain dotted filter
// (`user.created` → `user_created`) maps exactly as it always has and its
// existing consumer is reused. Every other character is escaped behind `-`,
// which therefore never appears unescaped:
//
//   - -> -s     _  -> -u     -  -> -d     other byte -> -xHH
//
// (Before this encoding `*` became a bare `s`, `_` and `-` a bare `x`, so
// `user.*` and `user.s` — or `a-b` and `a_b` — collided. Consumers for
// filters containing those characters are renamed by this change.)
func sanitizeDurableSuffix(filter string) string {
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(filter))
	for i := 0; i < len(filter); i++ {
		c := filter[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.':
			b.WriteByte('_')
		case c == '*':
			b.WriteString("-s")
		case c == '_':
			b.WriteString("-u")
		case c == '-':
			b.WriteString("-d")
		default:
			b.WriteString("-x")
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
