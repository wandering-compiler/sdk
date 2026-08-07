package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AwaitEvent is a step's optional assertion that a PUBLIC event arrives on
// the gateway's `/w17-events` SSE stream after the step's call succeeds.
//
// The event is asynchronous — it lands after the triggering RPC returns and
// its state change commits (see the server-tier emit contract). So a step
// carrying an AwaitEvent runs subscribe→trigger→await: the runner opens the
// stream BEFORE issuing the call (the stream has no replay; a late subscribe
// would miss an event that fired between the response and the subscribe),
// runs the call and asserts its response, then waits up to the timeout for
// the first frame on Topic and matches its payload.
type AwaitEvent struct {
	// Topic is the `(w17.event).topic` to await (e.g. "task.created"). It
	// is both the assertion discriminator and the `?topics=` filter the
	// runner subscribes with.
	Topic string

	// Path is the events route on the gateway — baked from the REST
	// surface prefix, e.g. "/api/v1/w17-events". Joined onto the REST base
	// URL the runner targets.
	Path string

	// TimeoutMs bounds the wait for the event. Zero → DefaultAwaitTimeoutMs.
	TimeoutMs int

	// Match asserts against the received event's decoded payload, using the
	// SAME matcher vocabulary as Step.Expect (not_empty, regex, count,
	// capture, nested, exact). An empty Match asserts only that the event
	// arrived. Capture matchers bind into the shared scenario scope, so a
	// later step can use an id the event carried.
	Match map[string]any
}

// DefaultAwaitTimeoutMs is the await window applied when
// AwaitEvent.TimeoutMs is zero.
const DefaultAwaitTimeoutMs = 5000

// Event is one delivered SSE event: its topic (the `event:` line) and the
// decoded JSON payload (the `data:` line).
type Event struct {
	Topic string
	Data  map[string]any
}

// EventSubscriber opens a live subscription to the gateway's public event
// stream. The generated runner wires an [SSESubscriber] built from the REST
// target; a test double implements this interface directly.
type EventSubscriber interface {
	// Subscribe opens a stream for topics with the bearer token (empty when
	// the surface has no auth). The returned Subscription is live — the
	// server-side bus subscription is registered — by the time Subscribe
	// returns, so the caller may trigger the event immediately after.
	Subscribe(ctx context.Context, path string, topics []string, token string) (Subscription, error)
}

// Subscription is a live event stream. Await blocks for the next event on a
// topic; Close releases the underlying connection.
type Subscription interface {
	Await(ctx context.Context, topic string, timeout time.Duration) (Event, error)
	Close() error
}

// SSESubscriber subscribes to the gateway's `/w17-events` SSE broadcast over
// HTTP. It is the production [EventSubscriber]; the generated runner builds
// one from the REST target URL.
type SSESubscriber struct {
	BaseURL string
	Client  *http.Client
}

// NewSSESubscriber builds an SSE subscriber against baseURL (the REST base,
// e.g. "http://localhost:8080"). A nil client uses a fresh timeout-less
// client — the stream is long-lived, so per-await deadlines (not a client
// timeout) bound the wait.
func NewSSESubscriber(baseURL string, client *http.Client) *SSESubscriber {
	if client == nil {
		client = &http.Client{}
	}
	return &SSESubscriber{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

// Subscribe opens GET <base><path>?topics=<t,...> with an SSE Accept header
// and the bearer token, and returns once the response headers arrive — at
// which point the gateway has already registered the bus subscription
// (HandleW17Events subscribes before flushing headers), so an event
// triggered next can't be missed.
func (s *SSESubscriber) Subscribe(ctx context.Context, path string, topics []string, token string) (Subscription, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	target := s.BaseURL + path + "?topics=" + url.QueryEscape(strings.Join(topics, ","))
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("await_event: build subscribe request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("await_event: subscribe %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("await_event: subscribe %s: status %d: %s", target, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	sub := &sseSub{
		cancel: cancel,
		body:   resp.Body,
		frames: make(chan Event, 16),
		done:   make(chan struct{}),
	}
	go sub.read()
	return sub, nil
}

// sseSub is one live SSE subscription: a reader goroutine parses frames off
// the response body into the frames channel; Await consumes them.
type sseSub struct {
	cancel    context.CancelFunc
	body      io.ReadCloser
	frames    chan Event
	done      chan struct{}
	closeOnce sync.Once
	readErr   error
}

// read parses SSE frames (`event:` / `data:` lines, blank-line separated)
// off the body until the stream ends or the subscription is closed. Comment
// lines (`:` heartbeats) are ignored.
func (s *sseSub) read() {
	defer close(s.frames)
	sc := bufio.NewScanner(s.body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var event, data string
	flush := func() {
		if event == "" && data == "" {
			return
		}
		var payload map[string]any
		if data != "" {
			_ = json.Unmarshal([]byte(data), &payload)
		}
		select {
		case s.frames <- Event{Topic: event, Data: payload}:
		case <-s.done:
		}
		event, data = "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// SSE comment / heartbeat — ignore.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(line[len("data:"):])
			if data == "" {
				data = d
			} else {
				data += "\n" + d
			}
		}
	}
	flush() // a final frame not terminated by a trailing blank line
	if err := sc.Err(); err != nil {
		s.readErr = err
	}
}

// Await blocks until the next event on topic arrives, the timeout fires, or
// the stream closes. Frames on other topics and heartbeats are skipped; an
// `error` frame is surfaced as a failure.
func (s *sseSub) Await(ctx context.Context, topic string, timeout time.Duration) (Event, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-deadline.C:
			return Event{}, fmt.Errorf("timeout after %s waiting for event on topic %q", timeout, topic)
		case ev, ok := <-s.frames:
			if !ok {
				if s.readErr != nil {
					return Event{}, fmt.Errorf("event stream closed: %w", s.readErr)
				}
				return Event{}, fmt.Errorf("event stream closed before an event on topic %q arrived", topic)
			}
			if ev.Topic == "error" {
				return Event{}, fmt.Errorf("event stream error frame: %v", ev.Data)
			}
			if ev.Topic == topic {
				return ev, nil
			}
			// A different topic (we asked for one, but stay tolerant of a
			// broadcast that carries siblings) — keep waiting.
		}
	}
}

// Close cancels the request and unblocks the reader. Idempotent.
func (s *sseSub) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		_ = s.body.Close()
	})
	return nil
}
