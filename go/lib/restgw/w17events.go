package restgw

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HandleW17Events is the SSE handler the gateway main mounts
// at `<RestApi.prefix>/w17-events` (the reserved pub/sub
// channel — see docs/specs/gateway/w17-events-channel.md).
// One open connection per client; the topics-filter query
// param selects which event topics the client cares about;
// each inbound bus event lands as one SSE frame:
//
//	event: <TopicName>
//	data: <protojson>
//
// Plus a comment-frame heartbeat (`: ping\n\n`) every
// `heartbeat` interval (defaults to 30s when zero) so
// intermediate proxies don't drop the connection on idle.
//
// **EventSource is pluggable.** C3.1 ships the handler +
// interface; C3.2 wires the real eventbus implementation.
// Tests + non-eventbus consumers (e.g. a hand-crafted
// admin diagnostic source) supply their own.
//
// **Lifecycle:**
//   - Parse `?topics=A,B,C` (empty / missing = no event
//     frames, heartbeat only — connection stays open as a
//     keep-alive).
//   - WriteSSEHeaders to flush headers immediately so the
//     client's EventSource onopen fires.
//   - source.Subscribe(ctx, topics) returns a receive channel
//     bound to the request context — channel closes on
//     client disconnect or source-side teardown.
//   - Loop: select { event in → write frame; heartbeat tick →
//     ping; ctx done → return; channel closed → return }.
//
// **Error reporting:** subscribe errors return a 502 before
// any SSE headers go out. Mid-stream errors are logged but
// don't propagate to the client (SSE has no error envelope
// shape; the connection just ends).
func HandleW17Events(w http.ResponseWriter, r *http.Request, source EventSource, heartbeat time.Duration) {
	if source == nil {
		http.Error(w, "w17-events: no event source bound", http.StatusInternalServerError)
		return
	}
	topics := parseTopicsParam(r.URL.Query().Get("topics"))
	ctx := r.Context()

	// The principal's AuthResp bytes (stashed by RequireAuth) carry
	// the labels used for per-principal broadcast filtering. nil on
	// the NoAuth path / direct (non-RequireAuth) calls — the source
	// then treats the principal as label-less (sees only unlabelled
	// events, which is every event today).
	userData := AuthUserDataFromContext(ctx)

	ch, err := source.Subscribe(ctx, topics, userData)
	if err != nil {
		http.Error(w, "w17-events: subscribe: "+err.Error(), http.StatusBadGateway)
		return
	}

	flusher, ok := WriteSSEHeaders(w)
	if !ok {
		// ResponseWriter doesn't support flush — proxies that
		// wrap the writer may surface this. Already wrote 200
		// via WriteSSEHeaders' WriteHeader call; client sees a
		// closed connection and reconnects per EventSource
		// semantics.
		return
	}

	if heartbeat <= 0 {
		heartbeat = 30 * time.Second
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	// Bound every frame/heartbeat write with a deadline. Without one a
	// slow/half-open client that stops reading but keeps the TCP
	// connection open fills the kernel send buffer; the write blocks
	// forever and r.Context() never cancels (no close is observed), so
	// the handler goroutine leaks — an unbounded-goroutine DoS vector.
	// Same mechanism as WriteSSEEventWithTimeout. SetWriteDeadline is
	// best-effort (some ResponseWriter wrappers don't support it).
	ctrl := http.NewResponseController(w)
	withWriteDeadline := func(write func() error) error {
		if defaultW17EventsWriteTimeout > 0 {
			_ = ctrl.SetWriteDeadline(time.Now().Add(defaultW17EventsWriteTimeout))
			defer func() { _ = ctrl.SetWriteDeadline(time.Time{}) }()
		}
		return write()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := withWriteDeadline(func() error { return writeHeartbeat(w, flusher) }); err != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := withWriteDeadline(func() error { return writeW17EventFrame(w, flusher, ev) }); err != nil {
				return
			}
		}
	}
}

// defaultW17EventsWriteTimeout bounds each SSE frame/heartbeat write on
// the reserved w17-events stream. Generous enough for slow links, short
// enough to reclaim a goroutine stuck on a half-open connection. (Kept
// internal to avoid changing the HandleW17Events signature, which is
// called from generated gateway code.)
const defaultW17EventsWriteTimeout = 30 * time.Second

// EventSource is the contract a w17-events handler needs from
// the underlying bus. Subscribe binds to the request context
// (channel must close when ctx cancels). The returned channel
// carries inbound events as JSON-encoded payloads — the
// source is responsible for filtering by topic.
//
// C3.1 ships only the interface + a test stub
// (NewStaticEventSource). C3.2 wires the real eventbus
// adapter.
//
// userData is the authenticated principal's marshaled AuthResp
// (from RequireAuth via [AuthUserDataFromContext]) — the source
// derives the principal's labels from it for per-principal
// broadcast filtering. nil = label-less principal (sees only
// unlabelled events).
type EventSource interface {
	Subscribe(ctx context.Context, topics []string, userData []byte) (<-chan Event, error)
}

// Event is one inbound bus payload destined for an SSE frame.
// Data is already JSON-encoded (protojson on the producer
// side); the handler writes it verbatim into the `data:`
// line.
type Event struct {
	Topic string
	Data  []byte
}

// StaticEventSource is the test-only EventSource that
// replays a fixed event list (filtered to the subscribed
// topics) and then closes the channel. Used by handler
// tests + by anyone wanting a hand-crafted feed without
// the real eventbus.
type StaticEventSource struct {
	Events []Event
}

// NewStaticEventSource wraps a slice for the EventSource
// interface.
func NewStaticEventSource(events ...Event) *StaticEventSource {
	return &StaticEventSource{Events: events}
}

// Subscribe writes every event whose Topic matches a
// subscribed topic onto a buffered channel, then closes it.
// Honours ctx cancellation between sends. The test stub ignores
// userData (no label filtering) — it's a topic-only feed.
func (s *StaticEventSource) Subscribe(ctx context.Context, topics []string, _ []byte) (<-chan Event, error) {
	wanted := map[string]struct{}{}
	for _, t := range topics {
		wanted[t] = struct{}{}
	}
	out := make(chan Event, len(s.Events))
	go func() {
		defer close(out)
		for _, ev := range s.Events {
			if _, ok := wanted[ev.Topic]; !ok {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}

// parseTopicsParam splits "A,B,C" into ["A", "B", "C"],
// dropping empty entries + trimming whitespace. Empty input
// returns nil (the handler subscribes to nothing in that
// case — heartbeat-only connection).
func parseTopicsParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// writeW17EventFrame writes one SSE frame in the
// `event: <topic>\ndata: <json>\n\n` shape this channel
// pins. Caller catches the io error on client disconnect.
func writeW17EventFrame(w http.ResponseWriter, flusher http.Flusher, ev Event) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, ev.Data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeHeartbeat emits a comment-line frame (per the SSE
// spec: lines starting with `:` are ignored by the client).
// Keeps the connection open through proxies + load
// balancers that close idle TCP connections.
func writeHeartbeat(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
