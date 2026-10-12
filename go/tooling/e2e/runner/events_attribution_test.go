package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// tenantHub is an SSE hub that delivers each event only to subscribers that
// authenticated as the event's principal, and records what every subscribe
// presented. POST /api/v1/tasks answers task_id t-<n> and, after `delay`,
// emits task.created for t-<n> to the caller's principal.
type tenantHub struct {
	mu       sync.Mutex
	subs     map[chan Event]string // subscriber → its Authorization
	n        int
	delays   []time.Duration // per trigger; the last repeats
	authSeen []string
}

func newTenantHub(delays ...time.Duration) *tenantHub {
	if len(delays) == 0 {
		delays = []time.Duration{20 * time.Millisecond}
	}
	return &tenantHub{subs: map[chan Event]string{}, delays: delays}
}

func (h *tenantHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		ch := make(chan Event, 8)
		h.mu.Lock()
		h.authSeen = append(h.authSeen, auth)
		h.subs[ch] = auth
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		}()
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				data := ev.Raw
				if data == "" {
					b, _ := json.Marshal(ev.Data)
					data = string(b)
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, data)
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		h.mu.Lock()
		h.n++
		id := fmt.Sprintf("t-%d", h.n)
		d := h.delays[min(h.n-1, len(h.delays)-1)]
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": id})
		go func() {
			time.Sleep(d)
			h.emit(auth, Event{Topic: "task.created", Data: map[string]any{"task_id": id}})
		}()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (h *tenantHub) emit(auth string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, a := range h.subs {
		if a != auth {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *tenantHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.authSeen...)
}

func runWith(t *testing.T, srv *httptest.Server, scope *runtime.Scope, steps ...Step) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return RunSteps(ctx, scope, steps,
		map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())},
		WithEventSubscriber(NewSSESubscriber(srv.URL, srv.Client())))
}

func sessionScope() *runtime.Scope {
	scope := runtime.NewRun().NewScope()
	scope.Capture("auth.token", "session")
	return scope
}

// A step that presents its own Credential observes events as THAT
// credential's principal. Subscribing with the scenario's session instead
// watched somebody else's stream: the await timed out on a surface that
// delivered correctly, or passed on an event the credential is never shown.
func TestAwaitEvent_CredentialStepSubscribesAsTheCredential(t *testing.T) {
	hub := newTenantHub()
	srv := hub.server(t)
	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 2000,
		Match: map[string]any{"task_id": "${task.id}"}})
	step.Endpoint.AuthRequired = true
	step.Credential = "ApiKey k-123"

	if err := runWith(t, srv, sessionScope(), step); err != nil {
		t.Fatalf("scenario: %v", err)
	}
	if seen := hub.seen(); len(seen) != 1 || seen[0] != "ApiKey k-123" {
		t.Fatalf("the subscription presented %q, want the step's credential", seen)
	}
}

// subscriberWithoutCredential is an EventSubscriber that can only present a
// bearer token.
type subscriberWithoutCredential struct{ EventSubscriber }

// A subscriber that cannot present a credential is refused for a credential
// step instead of quietly subscribing as the session.
func TestAwaitEvent_CredentialStepNeedsACredentialSubscriber(t *testing.T) {
	hub := newTenantHub()
	srv := hub.server(t)
	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 200})
	step.Endpoint.AuthRequired = true
	step.Credential = "ApiKey k-123"
	err := RunSteps(context.Background(), sessionScope(), []Step{step},
		map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())},
		WithEventSubscriber(subscriberWithoutCredential{NewSSESubscriber(srv.URL, srv.Client())}))
	if err == nil || !strings.Contains(err.Error(), "CredentialSubscriber") {
		t.Fatalf("err = %v, want a refusal naming CredentialSubscriber", err)
	}
	if seen := hub.seen(); len(seen) != 0 {
		t.Fatalf("subscribed anyway, presenting %q", seen)
	}
}

// An event that arrives late from an EARLIER step's call does not satisfy
// this step's await: it does not match what this step's call produced, so it
// is skipped and the await goes on to this call's own event.
func TestAwaitEvent_ALateEventFromAnEarlierStepIsNotThisStepsEvent(t *testing.T) {
	// Step 1's event lands while step 2 is waiting; step 2's lands after it.
	hub := newTenantHub(150*time.Millisecond, 400*time.Millisecond)
	srv := hub.server(t)
	first := createTaskStep(nil)
	first.Expect = map[string]any{"task_id": map[string]any{"capture": "first.id"}}
	second := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 2000,
		Match: map[string]any{"task_id": "${task.id}"}})
	second.Endpoint.AuthRequired = true
	first.Endpoint.AuthRequired = true

	scope := sessionScope()
	if err := runWith(t, srv, scope, first, second); err != nil {
		t.Fatalf("the earlier step's event was taken for this step's: %v", err)
	}
	if v, _ := scope.Get("task.id"); v != "t-2" {
		t.Fatalf("task.id = %v, want t-2", v)
	}
}

// A frame delivered before the step's call was sent is never this call's,
// even under a match that asserts nothing.
func TestSSESubscription_IgnoresFramesBeforeTheMark(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ev", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: task.created\ndata: {\"task_id\":\"old\"}\n\n")
		fl.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sub, err := NewSSESubscriber(srv.URL, srv.Client()).Subscribe(context.Background(), "/ev", []string{"task.created"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	time.Sleep(200 * time.Millisecond) // the early frame is parsed by now
	sub.(Settable).IgnoreBefore(time.Now())
	if ev, err := sub.Await(context.Background(), "task.created", 200*time.Millisecond); err == nil {
		t.Fatalf("a frame from before the mark was returned: %v", ev.Data)
	}
}

// A suite can assert that an event is NOT delivered: here the other tenant
// subscribes (TokenFrom) and must see nothing of this step's event, while
// the same assertion made as the emitting principal fails.
func TestAwaitEvent_AbsentAssertsNonDelivery(t *testing.T) {
	hub := newTenantHub()
	srv := hub.server(t)
	scope := sessionScope()
	scope.Capture("bob.token", "bob")

	other := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 300,
		Absent: true, TokenFrom: "bob.token"})
	other.Endpoint.AuthRequired = true
	if err := runWith(t, srv, scope, other); err != nil {
		t.Fatalf("another tenant's silence was not accepted: %v", err)
	}
	if seen := hub.seen(); len(seen) != 1 || seen[0] != "Bearer bob" {
		t.Fatalf("the absent await subscribed as %q, want the TokenFrom principal", seen)
	}

	self := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 300, Absent: true})
	self.Endpoint.AuthRequired = true
	err := runWith(t, srv, scope, self)
	if err == nil || !strings.Contains(err.Error(), "WAS delivered") {
		t.Fatalf("a delivered event passed an absent assertion: %v", err)
	}
}

// An absent await whose match tries to capture is refused: nothing will be
// delivered to bind from.
func TestAwaitEvent_AbsentCannotCapture(t *testing.T) {
	hub := newTenantHub()
	srv := hub.server(t)
	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 100, Absent: true,
		Match: map[string]any{"task_id": map[string]any{"capture": "x"}}})
	step.Endpoint.AuthRequired = true
	if err := runWith(t, srv, sessionScope(), step); err == nil || !strings.Contains(err.Error(), "capture") {
		t.Fatalf("err = %v, want a refusal of the capture", err)
	}
}

// Headers on an await replace the step's on that subscription.
func TestAwaitEvent_AwaitHeadersReplaceTheSteps(t *testing.T) {
	got := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("W17-Org"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	scope := sessionScope()
	scope.Capture("bob.org", "org-b")
	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 100, Absent: true,
		Headers: map[string]string{"W17-Org": "${bob.org}"}})
	step.Headers = map[string]string{"W17-Org": "org-a"}
	if err := runWith(t, srv, scope, step); err != nil {
		t.Fatalf("scenario: %v", err)
	}
	if org := <-got; org != "org-b" {
		t.Fatalf("subscription W17-Org = %q, want the await's own org-b", org)
	}
}

// A frame whose data is not JSON fails the step. Read as an empty payload it
// satisfied any match that asserted nothing — a pass over a frame nobody
// could read.
func TestAwaitEvent_NonJSONDataFailsTheStep(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: task.created\ndata: <html>oops</html>\n\n")
		fl.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 2000})
	err := runWith(t, srv, sessionScope(), step)
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("a non-JSON event frame = %v, want the step to fail naming it", err)
	}
}

// The same holds for an endpoint's own stream.
func TestExpectStream_NonJSONFrameFailsTheStep(t *testing.T) {
	srv := streamServer(t, []string{`not json`}, false, 0)
	err := runStream(t, srv, streamStep(&ExpectStream{Frames: []map[string]any{{}}}))
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("a non-JSON stream frame = %v, want the step to fail naming it", err)
	}
}
