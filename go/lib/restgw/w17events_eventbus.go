package restgw

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// EventbusEventSource is the EventSource implementation that
// reads from a project's eventbus and forwards filtered
// deliveries to the w17-events SSE handler. C3.2 ship — the
// adapter is generic; the per-domain envelope decoding goes
// through the caller-supplied TranscodeFunc so this package
// stays domain-agnostic.
//
// **Shared fan-out hub (1 bus subscriber per channel → N SSE
// clients).** The source opens ONE bus Subscriber per configured
// channel — lazily on the first connecting client — and fans
// every delivery out to all currently-connected SSE clients,
// filtered per-client by the topic set each one asked for. A
// domain whose public events span several (broker) channels
// composes them here: every channel feeds the SAME hub, and the
// domain-wide transcode dispatches each delivery by its topic
// regardless of which channel carried it. This mirrors the
// original devlog gateway's BroadcastHub: a small fixed number of
// long-lived bus subscriptions (one per channel) regardless of
// how many browser tabs are open, instead of one subscription per
// client. Each delivery is transcoded ONCE (not once per client)
// and the resulting frame is fanned out.
//
// The shared subscriber is opened against context.Background()
// (it outlives any single request) and is never torn down for
// the source's lifetime — the source is constructed once in the
// generated gateway main and lives for the process. When zero
// clients are connected the handler still runs but finds nobody
// interested, so the cost is one idle subscription. (The
// original additionally drained the subscription on SIGTERM;
// this read-only fan-out has nothing to ack, so the bus
// subscription simply dies with the process — a deliberate
// simplification.)
//
// **Subscribe semantics.** Per the eventbus convention (see
// docs/specs/eventbus/subscribers-registry.md), the adapter
// Subscribes with the catch-all `**` topic filter and re-checks
// each client's requested topic set during fan-out. Keeps the
// per-channel Subscribe count at one.
//
// **Transcode contract.** The eventbus delivers raw envelope
// bytes (proto-encoded, domain-specific wrapper type). The
// TranscodeFunc unmarshals the envelope, extracts the inner
// event payload, and re-marshals as protojson — the shape
// the FE expects. Generated gateway main (C3.3) supplies the
// per-domain transcode; tests + non-generated callers can
// supply a stub (e.g. identity for raw-passthrough scenarios).
type EventbusEventSource struct {
	factory   eventbus.SubscriberFactory
	channels  []string
	transcode TranscodeFunc
	principal PrincipalLabelsFunc

	// openMu serializes lazy-open attempts. It is deliberately NOT mu: the
	// open path talks to the broker and, when an attempt fails, drains what
	// it opened — and a drain waits for in-flight handler callbacks, which
	// take mu. Holding mu across either would make the hub's control path
	// wait on its own data path. See ensureOpen.
	openMu sync.Mutex

	mu      sync.Mutex
	opened  bool
	subs    []eventbus.Subscriber
	clients map[*hubClient]struct{}
}

// hubClient is one connected SSE client: the topic set it asked
// for, the principal's labels (for per-principal broadcast
// filtering), plus the channel deliveries are pushed onto. out
// is closed by the per-client cleanup goroutine once the request
// context cancels (the client disconnected).
type hubClient struct {
	wanted map[string]struct{}
	labels map[string]string
	out    chan Event
}

// hubClientBuffer bounds each client's pending-frame queue.
// Fan-out is non-blocking: when a client's buffer is full the
// frame is dropped FOR THAT CLIENT only, so one slow consumer
// can never stall the shared hub (and thereby every other
// client). SSE is fire-and-forget — a dropped frame is a missed
// event, not a corrupted stream; dashboards re-fetch state.
const hubClientBuffer = 64

// TranscodeFunc turns one raw envelope delivery into the
// per-event JSON payload + the event's broadcast labels. Returns
// (skip=true) when the topic shouldn't be forwarded (e.g.
// a delivery the handler doesn't recognise); the source
// drops without erroring. Returns (err) when the envelope
// is corrupt — the source logs + drops; the SSE connection
// stays open.
//
// labels are the event's tenant/scope labels (envelope.labels,
// stamped at emit time). The source delivers the frame to a
// connected client only when every event label is satisfied by
// the client's principal labels (see [PrincipalLabelsFunc] +
// labelsSubset). Empty/nil event labels = broadcast to every
// authenticated client (the single-tenant default).
type TranscodeFunc func(envelopeTopic string, envelope []byte) (payloadJSON []byte, labels map[string]string, skip bool, err error)

// PrincipalLabelsFunc derives a connected client's principal
// labels from its marshaled AuthResp bytes (userData from
// RequireAuth). The generated gateway supplies the domain
// specific implementation (proto.Unmarshal the AuthResp →
// AuthResp.Labels); a nil func or nil/empty userData yields a
// label-less principal — it then receives only unlabelled events.
type PrincipalLabelsFunc func(userData []byte) map[string]string

// NewEventbusEventSource wires the adapter against a
// SubscriberFactory + the set of channels that carry the domain's
// public events (+ the invalidate relay) + the domain-specific
// transcode. Callers (generated gateway main) pass every channel
// the FE's events are emitted on; the hub opens one bus
// subscriber per channel and composes them into a single SSE
// surface (one /w17-events route), so a public event on any of
// the domain's channels reaches the browser. The channel list
// should be non-empty and deterministically ordered by the
// caller.
// principal may be nil — every connected client is then
// label-less and the broadcast degrades to topic-only filtering
// (every authenticated client sees every public event, the
// single-tenant posture).
func NewEventbusEventSource(factory eventbus.SubscriberFactory, channels []string, transcode TranscodeFunc, principal PrincipalLabelsFunc) *EventbusEventSource {
	return &EventbusEventSource{
		factory:   factory,
		channels:  channels,
		transcode: transcode,
		principal: principal,
		clients:   map[*hubClient]struct{}{},
	}
}

// Subscribe implements EventSource.Subscribe. Registers a new
// client with the shared hub (opening the single bus subscriber
// on the first client), returns the client's receive channel,
// and spawns a cleanup goroutine that deregisters + closes the
// channel when the request context cancels.
func (e *EventbusEventSource) Subscribe(ctx context.Context, topics []string, userData []byte) (<-chan Event, error) {
	if e.factory == nil {
		return nil, fmt.Errorf("eventbus source: nil factory")
	}
	if e.transcode == nil {
		return nil, fmt.Errorf("eventbus source: nil transcode")
	}
	if err := e.ensureOpen(); err != nil {
		return nil, err
	}

	wanted := make(map[string]struct{}, len(topics))
	for _, t := range topics {
		wanted[t] = struct{}{}
	}
	var labels map[string]string
	if e.principal != nil {
		labels = e.principal(userData)
	}
	c := &hubClient{wanted: wanted, labels: labels, out: make(chan Event, hubClientBuffer)}

	e.mu.Lock()
	e.clients[c] = struct{}{}
	e.mu.Unlock()

	go func() {
		<-ctx.Done()
		e.mu.Lock()
		delete(e.clients, c)
		e.mu.Unlock()
		// Safe to close outside the lock: deliveries push to
		// c.out only while holding e.mu, and the delete above
		// (under the same lock) happens-before this close — so
		// no fan-out can send to c.out after it's deleted.
		close(c.out)
	}()
	return c.out, nil
}

// openDrainTimeout bounds the cleanup drain of a FAILED open attempt.
// The drain's stop half — cancelling the consume loops — is synchronous and
// already done when the wait begins; only the wait for callbacks that are
// already inside a handler is bounded here. Without a bound one wedged
// handler would pin the open path (and therefore every connecting client)
// for the process lifetime.
const openDrainTimeout = 10 * time.Second

// ensureOpen lazily opens one shared bus subscriber per channel
// on the first client. `opened` flips true only after EVERY
// channel opened, so a transient subscribe failure lets the next
// connecting client retry the whole set (rather than wedging the
// surface for the process lifetime). All channels feed the same
// handleDelivery, so deliveries from every channel land in the one
// shared hub.
//
// **Nothing here runs under e.mu.** e.mu guards the client set, and the
// hub's own bus handler takes it on every delivery (handleDelivery →
// anyInterested). The open path does broker I/O, and on failure it drains
// what it opened — and both shipped adapters' Drain waits for in-flight
// handler callbacks. Draining under e.mu therefore closed a cycle: the
// drain waited for a delivery that was waiting for e.mu, with no timeout
// on either side, so one transient subscribe failure coinciding with one
// delivery in flight held the client-set lock for the rest of the process.
// Every later /w17-events request and every delivery blocked behind it.
// openMu serializes the attempts instead, so concurrent first clients still
// open the set exactly once while deliveries keep flowing throughout.
func (e *EventbusEventSource) ensureOpen() error {
	if e.isOpened() {
		return nil
	}
	if len(e.channels) == 0 {
		return fmt.Errorf("eventbus source: no channels configured")
	}

	e.openMu.Lock()
	defer e.openMu.Unlock()
	// Re-check: a concurrent attempt may have opened the set while we queued.
	if e.isOpened() {
		return nil
	}

	subs := make([]eventbus.Subscriber, 0, len(e.channels))
	// Retrying the whole set is only cheap if a failed attempt leaves nothing
	// behind. Channels opened before the failing one are already subscribed
	// and their consume loops are already running; dropping the slice on the
	// floor leaks one set of them per retry, and a broker outage retries once
	// per reconnecting client.
	drainAll := func() {
		ctx, cancel := context.WithTimeout(context.Background(), openDrainTimeout)
		defer cancel()
		for _, sub := range subs {
			_ = sub.Drain(ctx)
		}
	}
	for _, channel := range e.channels {
		sub, err := e.factory.Subscriber(context.Background(), channel)
		if err != nil {
			drainAll()
			return fmt.Errorf("eventbus source: subscriber for channel %q: %w", channel, err)
		}
		if err := sub.Subscribe(context.Background(), "**", e.handleDelivery); err != nil {
			// Drain this one too: Subscriber() handed back a live handle
			// whether or not the subscription took.
			subs = append(subs, sub)
			drainAll()
			return fmt.Errorf("eventbus source: subscribe on channel %q: %w", channel, err)
		}
		subs = append(subs, sub)
	}

	e.mu.Lock()
	e.subs = subs
	e.opened = true
	e.mu.Unlock()
	return nil
}

// isOpened reads the lazy-open flag under the client-set lock. Split out so
// the open path can consult it without holding that lock across any broker
// call or drain.
func (e *EventbusEventSource) isOpened() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opened
}

// handleDelivery is the single shared bus handler. Skips the
// transcode entirely when no connected client wants the topic,
// transcodes once otherwise, then fans the frame out to every
// interested client. Returning nil acks the delivery (the bus
// protocol is at-least-once / fire-and-forget at this layer —
// there's no error envelope into the FE).
func (e *EventbusEventSource) handleDelivery(_ context.Context, topic string, envelope []byte) error {
	if !e.anyInterested(topic) {
		return nil
	}
	payload, labels, skip, err := e.transcode(topic, envelope)
	if err != nil || skip {
		return nil
	}
	e.fanOut(topic, labels, Event{Topic: topic, Data: payload})
	return nil
}

// anyInterested reports whether at least one connected client
// subscribed to `topic` — a cheap gate so an unwanted delivery
// skips the (potentially non-trivial) transcode.
func (e *EventbusEventSource) anyInterested(topic string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for c := range e.clients {
		if _, ok := c.wanted[topic]; ok {
			return true
		}
	}
	return false
}

// fanOut pushes one already-transcoded frame to every client
// subscribed to its topic AND entitled to it — the principal's
// labels must satisfy every event label (labelsSubset). Non-
// blocking per client (drop on full buffer); the send happens
// under e.mu so it can never race the close in the per-client
// cleanup goroutine.
func (e *EventbusEventSource) fanOut(topic string, eventLabels map[string]string, ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Label-based tenant isolation is active when a principal-labels
	// deriver was configured.
	isolation := e.principal != nil
	for c := range e.clients {
		if _, ok := c.wanted[topic]; !ok {
			continue
		}
		if !entitled(isolation, eventLabels, c.labels) {
			continue
		}
		select {
		case c.out <- ev:
		default:
			// Buffer full — drop for this client only.
		}
	}
}

// entitled decides whether a connected principal receives an event.
//
// mcpbus-sec-1: when tenant isolation is active (a principal-labels
// deriver is configured — i.e. the surface declares the labels field it
// partitions by), an UNLABELLED event is delivered to NOBODY. It cannot
// be scoped to any tenant, so there is no principal it can be shown to
// safely. A LABELLED event keeps the subset rule; with isolation off
// (no deriver — the single-tenant default) an unlabelled event still
// broadcasts to every connected client.
//
// The rule got here in two steps, and both halves matter:
//
//  1. `labelsSubset(empty, …)` was vacuously true, so any event emitted
//     without labels — notably the reserved `w17.invalidate`, which
//     carries entity IDs — leaked to every connected tenant.
//  2. Withholding it from LABEL-BEARING principals only (delivering it
//     to the label-less ones) inverted the fail-closed branch it shares
//     with the row side: a scope decorator that cannot resolve an
//     unambiguous value deliberately stamps neither scope nor label, so
//     a scoped model refuses for that caller — and this consumer then
//     made that same caller the one audience for every unlabelled event
//     in the deployment. `len(orgs) != 1` includes ZERO memberships, so
//     the least-privileged principal was the widest audience (C8-F11).
//     Failing to resolve must never widen delivery.
//
// Consequence: on an isolating surface every event has to carry the
// label it is partitioned by — including cross-tenant cache
// invalidation, which is why [eventbus.EmitInvalidate] labels
// InvalidateEvent from the emitting request's context. An event emitted
// outside any labelled request reaches nobody, by design: that is the
// fail-closed direction, and it is the same answer the row side gives.
func entitled(isolation bool, eventLabels, principalLabels map[string]string) bool {
	if isolation && len(eventLabels) == 0 {
		return false
	}
	return labelsSubset(eventLabels, principalLabels)
}

// labelsSubset reports whether every (key,value) in eventLabels is
// present with the same value in principalLabels — a principal is
// entitled to a labelled event iff it "possesses" all of the event's
// labels. (See [entitled] for the unlabelled-event isolation rule.)
func labelsSubset(eventLabels, principalLabels map[string]string) bool {
	for k, v := range eventLabels {
		if principalLabels[k] != v {
			return false
		}
	}
	return true
}
