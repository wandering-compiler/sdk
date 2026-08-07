package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestHandleW17Events_FramesFilteredByTopic — only events
// whose Topic matches a `?topics=` entry land on the wire.
// Frame shape: `event: <Topic>\ndata: <Data>\n\n`.
func TestHandleW17Events_FramesFilteredByTopic(t *testing.T) {
	source := restgw.NewStaticEventSource(
		restgw.Event{Topic: "AccountUpdated", Data: []byte(`{"id":"a-1"}`)},
		restgw.Event{Topic: "NoiseTopic", Data: []byte(`{"x":1}`)},
		restgw.Event{Topic: "TaskCompleted", Data: []byte(`{"id":"t-9"}`)},
	)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/w17-events?topics=AccountUpdated,TaskCompleted", nil)
	rec := httptest.NewRecorder()

	restgw.HandleW17Events(rec, req, source, 1*time.Hour) // heartbeat far in the future

	body := rec.Body.String()
	for _, want := range []string{
		"event: AccountUpdated\ndata: {\"id\":\"a-1\"}\n\n",
		"event: TaskCompleted\ndata: {\"id\":\"t-9\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "NoiseTopic") {
		t.Errorf("non-subscribed topic leaked into body:\n%s", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

// TestHandleW17Events_EmptyTopicsKeepsConnectionOpen — no
// `?topics=` query means nothing to subscribe; handler still
// opens the SSE headers + waits for heartbeat / ctx cancel.
// Uses a blocking source + short heartbeat + ctx cancellation
// to bound the wait.
func TestHandleW17Events_EmptyTopicsKeepsConnectionOpen(t *testing.T) {
	source := &blockingSource{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 20*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler did not return after ctx cancel")
	}

	body := rec.Body.String()
	if !strings.Contains(body, ": ping\n\n") {
		t.Errorf("expected at least one heartbeat frame; got:\n%s", body)
	}
}

// TestHandleW17Events_HeartbeatEvery — heartbeat ticks at
// the configured interval until ctx cancel; we drive a short
// ctx + count ping frames. Uses blockingSource so the
// channel stays open (StaticEventSource closes after the
// fixed event list drains, which would exit the handler
// before any heartbeat fired).
func TestHandleW17Events_HeartbeatEvery(t *testing.T) {
	source := &blockingSource{}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=X", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	restgw.HandleW17Events(rec, req, source, 20*time.Millisecond)

	pings := strings.Count(rec.Body.String(), ": ping\n\n")
	if pings < 2 {
		t.Errorf("expected at least 2 heartbeat ticks in 70ms window @ 20ms interval; got %d\n--- body ---\n%s",
			pings, rec.Body.String())
	}
}

// TestHandleW17Events_NoSource — passing nil EventSource
// returns 500 with a clear diag, never opens SSE headers.
func TestHandleW17Events_NoSource(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events", nil)
	rec := httptest.NewRecorder()

	restgw.HandleW17Events(rec, req, nil, 0)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no event source") {
		t.Errorf("body should mention missing source; got: %q", rec.Body.String())
	}
}

// TestHandleW17Events_SubscribeError — source.Subscribe
// returning an error surfaces as 502 with the error text,
// no SSE headers.
func TestHandleW17Events_SubscribeError(t *testing.T) {
	source := &failingSource{err: "topic catalog unavailable"}
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=X", nil)
	rec := httptest.NewRecorder()

	restgw.HandleW17Events(rec, req, source, 0)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "topic catalog unavailable") {
		t.Errorf("body should carry the subscribe error; got: %q", rec.Body.String())
	}
}

// TestHandleW17Events_CtxCancelEndsHandler — request ctx
// cancellation mid-stream unwinds the handler within one
// heartbeat tick.
func TestHandleW17Events_CtxCancelEndsHandler(t *testing.T) {
	// Readiness-signalled instead of sleep-synced (R-restgw-6): the
	// source closes `subscribed` the instant the handler enters its
	// streaming loop, so we cancel deterministically the moment the
	// handler is actually streaming — no fixed delay to tune or flake.
	subscribed := make(chan struct{})
	source := &signalingSource{subscribed: subscribed}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=X", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		restgw.HandleW17Events(rec, req, source, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never subscribed")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after ctx cancel")
	}
}

// TestParseTopics_Whitespace — `?topics=` parser trims +
// drops empty entries, preserves declared order.
func TestParseTopics_Whitespace(t *testing.T) {
	cases := []struct {
		raw   string
		topic string
		want  bool
	}{
		{"A,B,C", "A", true},
		{"A,B,C", "B", true},
		{"A,B,C", "D", false},
		{url.QueryEscape(" A , B , C "), "A", true},
		{url.QueryEscape(" A , B , C "), "B", true},
		{",,A,,", "A", true},
		{",,A,,", "", false},
	}
	for _, tc := range cases {
		source := restgw.NewStaticEventSource(
			restgw.Event{Topic: tc.topic, Data: []byte(`{}`)},
		)
		req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics="+tc.raw, nil)
		rec := httptest.NewRecorder()
		restgw.HandleW17Events(rec, req, source, 1*time.Hour)

		got := strings.Contains(rec.Body.String(), "event: "+tc.topic+"\n")
		if got != tc.want {
			t.Errorf("topics=%q expecting %q matched=%v want=%v", tc.raw, tc.topic, got, tc.want)
		}
	}
}

// --- helpers ---

// blockingSource returns a channel that stays open until ctx
// cancellation — never sends anything. Used by heartbeat +
// keep-alive tests where StaticEventSource's "drain + close"
// semantics would exit the handler too early.
type blockingSource struct{}

func (b *blockingSource) Subscribe(ctx context.Context, _ []string, _ []byte) (<-chan restgw.Event, error) {
	out := make(chan restgw.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

// signalingSource closes `subscribed` when the handler subscribes,
// then behaves like blockingSource (stays open until ctx cancel).
// Lets tests sync on the handler reaching its streaming loop instead
// of sleeping. close panics if Subscribe is called twice, which is
// fine — these tests subscribe exactly once.
type signalingSource struct {
	subscribed chan struct{}
}

func (s *signalingSource) Subscribe(ctx context.Context, _ []string, _ []byte) (<-chan restgw.Event, error) {
	close(s.subscribed)
	out := make(chan restgw.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

type failingSource struct {
	err string
}

func (f *failingSource) Subscribe(_ context.Context, _ []string, _ []byte) (<-chan restgw.Event, error) {
	return nil, &stringError{msg: f.err}
}

type stringError struct {
	msg string
	mu  sync.Mutex
}

func (e *stringError) Error() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.msg
}
