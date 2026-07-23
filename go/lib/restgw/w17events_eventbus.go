package restgw

import (
	"context"
	"fmt"
	"sync"

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

// ensureOpen lazily opens one shared bus subscriber per channel
// on the first client. `opened` flips true only after EVERY
// channel opened, so a transient subscribe failure lets the next
// connecting client retry the whole set (rather than wedging the
// surface for the process lifetime). Holds e.mu across
// factory.Subscriber + Subscribe; both register-and-return
// (deliveries arrive async on other goroutines), so the lock hold
// is bounded. All channels feed the same handleDelivery, so
// deliveries from every channel land in the one shared hub.
func (e *EventbusEventSource) ensureOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opened {
		return nil
	}
	if len(e.channels) == 0 {
		return fmt.Errorf("eventbus source: no channels configured")
	}
	subs := make([]eventbus.Subscriber, 0, len(e.channels))
	for _, channel := range e.channels {
		sub, err := e.factory.Subscriber(context.Background(), channel)
		if err != nil {
			return fmt.Errorf("eventbus source: subscriber for channel %q: %w", channel, err)
		}
		if err := sub.Subscribe(context.Background(), "**", e.handleDelivery); err != nil {
			return fmt.Errorf("eventbus source: subscribe on channel %q: %w", channel, err)
		}
		subs = append(subs, sub)
	}
	e.subs = subs
	e.opened = true
	return nil
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
// deriver is configured), an UNLABELLED event is delivered ONLY to
// principals that themselves carry no labels — it is NOT broadcast to
// label-bearing (tenant) principals, because it can't be scoped to
// their tenant. Previously `labelsSubset(empty, …)` was vacuously true,
// so any event emitted without labels (notably the reserved, always-
// unlabelled `w17.invalidate`, which carries entity IDs) leaked to every
// connected tenant. A LABELLED event keeps the subset rule.
//
// Consequence: in a multi-tenant surface, an event meant for all tenants
// must carry the tenant label (or be re-emitted per tenant) — an
// unlabelled "global" broadcast no longer reaches tenant principals.
// This is the secure default; cross-tenant cache invalidation must label
// its events per tenant.
func entitled(isolation bool, eventLabels, principalLabels map[string]string) bool {
	if isolation && len(eventLabels) == 0 {
		return len(principalLabels) == 0
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
