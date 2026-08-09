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

// eventHub is a tiny in-test SSE broadcaster: subscribers register a channel
// on GET /api/v1/w17-events; POST /api/v1/tasks answers the trigger and fans
// an event out to every live subscriber — exercising the real
// subscribe→trigger→deliver ordering the runner relies on.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newEventHub() *eventHub { return &eventHub{subs: map[chan Event]struct{}{}} }

func (h *eventHub) add() chan Event {
	ch := make(chan Event, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) remove(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *eventHub) broadcast(ev Event) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *eventHub) server(t *testing.T, broadcastOnTrigger bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("SSE ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush() // headers out now → Subscribe returns, subscription live
		ch := h.add()
		defer h.remove(ch)
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				payload, _ := json.Marshal(ev.Data)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, payload)
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
		if broadcastOnTrigger {
			// Tiny delay so the event genuinely lands AFTER the response —
			// the case the runner must not race.
			go func() {
				time.Sleep(20 * time.Millisecond)
				h.broadcast(Event{Topic: "task.created", Data: map[string]any{
					"task_id": "t-1", "title": body["title"],
				}})
			}()
		}
	})
	return httptest.NewServer(mux)
}

func createTaskStep(await *AwaitEvent) Step {
	s := Step{
		Endpoint: Endpoint{
			Ref: "tasks.TaskMutation.CreateTask", Transport: "rest",
			HTTPMethod: "POST", PathTemplate: "/api/v1/tasks",
		},
		Input:  map[string]any{"title": "hello"},
		Expect: map[string]any{"task_id": map[string]any{"capture": "task.id"}},
		Label:  "0010_create_task.yaml",
	}
	if await != nil {
		s.AwaitEvents = []AwaitEvent{*await}
	}
	return s
}

// TestAwaitEvent_HappyPath — a step triggers a REST call whose async public
// event lands on /w17-events shortly after the response; the runner
// subscribes first, matches the payload (including a capture from the
// response), and passes.
func TestAwaitEvent_HappyPath(t *testing.T) {
	hub := newEventHub()
	srv := hub.server(t, true)
	defer srv.Close()

	steps := []Step{createTaskStep(&AwaitEvent{
		Topic: "task.created",
		Path:  "/api/v1/w17-events",
		Match: map[string]any{
			"task_id": "${task.id}", // cross-checks the response capture
			"title":   map[string]any{"matcher": "not_empty"},
		},
	})}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	err := RunScenario(context.Background(), steps, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil)))
	if err != nil {
		t.Fatalf("scenario failed: %v", err)
	}
}

// TestAwaitEvents_Multiple — a step awaits TWO public events from one call;
// both land (in reversed order) and both match, exercising the per-topic
// subscription that keeps one Await from eating the other's frame.
func TestAwaitEvents_Multiple(t *testing.T) {
	hub := newEventHub()
	// A dedicated server whose trigger fans out two events (reversed order).
	srv := httptest.NewServer(func() http.Handler {
		m := http.NewServeMux()
		m.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
			fl := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl.Flush()
			ch := hub.add()
			defer hub.remove(ch)
			for {
				select {
				case <-r.Context().Done():
					return
				case ev := <-ch:
					payload, _ := json.Marshal(ev.Data)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, payload)
					fl.Flush()
				}
			}
		})
		m.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
			go func() {
				time.Sleep(15 * time.Millisecond)
				hub.broadcast(Event{Topic: "task.indexed", Data: map[string]any{"task_id": "t-1"}})
				hub.broadcast(Event{Topic: "task.created", Data: map[string]any{"task_id": "t-1"}})
			}()
		})
		return m
	}())
	defer srv.Close()

	step := createTaskStep(nil)
	step.AwaitEvents = []AwaitEvent{
		{Topic: "task.created", Path: "/api/v1/w17-events", Match: map[string]any{"task_id": "${task.id}"}},
		{Topic: "task.indexed", Path: "/api/v1/w17-events", Match: map[string]any{"task_id": map[string]any{"matcher": "not_empty"}}},
	}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	if err := RunScenario(context.Background(), []Step{step}, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil))); err != nil {
		t.Fatalf("multi-await scenario failed: %v", err)
	}
}

// TestAwaitEvent_Timeout — no event is broadcast, so the await times out
// with a clear diagnostic (and the scenario fails).
func TestAwaitEvent_Timeout(t *testing.T) {
	hub := newEventHub()
	srv := hub.server(t, false)
	defer srv.Close()

	steps := []Step{createTaskStep(&AwaitEvent{
		Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 200,
	})}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	err := RunScenario(context.Background(), steps, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil)))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") || !strings.Contains(err.Error(), "task.created") {
		t.Errorf("error = %q, want a timeout on topic task.created", err)
	}
}

// TestAwaitEvent_PayloadMismatch — the event arrives but its payload fails
// the Match, so the step fails with an await_event payload error (not a
// timeout).
func TestAwaitEvent_PayloadMismatch(t *testing.T) {
	hub := newEventHub()
	srv := hub.server(t, true)
	defer srv.Close()

	steps := []Step{createTaskStep(&AwaitEvent{
		Topic: "task.created", Path: "/api/v1/w17-events",
		Match: map[string]any{"task_id": "does-not-match"},
	})}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	err := RunScenario(context.Background(), steps, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil)))
	if err == nil {
		t.Fatal("expected payload mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "await_event") || !strings.Contains(err.Error(), "payload") {
		t.Errorf("error = %q, want an await_event payload mismatch", err)
	}
}

// TestAwaitEvent_StepHeadersAuthenticateTheStream — a step's `headers:` ride
// the SSE subscribe, not just the call.
//
// The server here is the shape a tenant-partitioned gateway actually has:
// the events route reads `W17-Org` (which the real gateway forwards to the
// auth method, whose AuthResp.labels the hub partitions by) and a subscriber
// that named no org is entitled to NOTHING — the fail-closed rule
// restgw.entitled applies to an unlabelled principal. So a subscribe that
// dropped the step's headers connects as a different, label-less principal
// than the call it is meant to observe, and the await times out on a surface
// that works. Revert the header pass-through and this test fails on the
// timeout, not on the payload.
func TestAwaitEvent_StepHeadersAuthenticateTheStream(t *testing.T) {
	var mu sync.Mutex
	subs := map[chan Event]string{} // subscriber → the org it subscribed as

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		ch := make(chan Event, 4)
		mu.Lock()
		subs[ch] = r.Header.Get("W17-Org")
		mu.Unlock()
		defer func() { mu.Lock(); delete(subs, ch); mu.Unlock() }()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				payload, _ := json.Marshal(ev.Data)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, payload)
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		org := r.Header.Get("W17-Org")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
		go func() {
			time.Sleep(15 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			for ch, subOrg := range subs {
				// The event carries the emitting request's org label; an
				// unlabelled principal (subOrg == "") satisfies no label.
				if subOrg != org {
					continue
				}
				select {
				case ch <- Event{Topic: "task.created", Data: map[string]any{"task_id": "t-1", "org": org}}:
				default:
				}
			}
		}()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	step := createTaskStep(&AwaitEvent{
		Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 1500,
		Match: map[string]any{"org": "acme"},
	})
	step.Headers = map[string]string{"W17-Org": "acme"}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	if err := RunScenario(context.Background(), []Step{step}, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil))); err != nil {
		t.Fatalf("scenario failed: %v", err)
	}
}

// TestAwaitEvent_SubscribeKeepsItsOwnCredential — a `headers:` entry cannot
// override the stream's Authorization or its SSE Accept. The step names both;
// the server asserts the built-ins survived (otherwise a scenario could
// authenticate its stream as somebody else, or ask for a non-SSE body and
// hang).
func TestAwaitEvent_SubscribeKeepsItsOwnCredential(t *testing.T) {
	got := make(chan [2]string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- [2]string{r.Header.Get("Authorization"), r.Header.Get("Accept")}:
		default:
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	step := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 100})
	step.Endpoint.AuthRequired = true
	step.Headers = map[string]string{"Authorization": "Bearer forged", "Accept": "application/json"}
	scope := runtime.NewRun().NewScope()
	scope.Capture("auth.token", "real-token")
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	// The await itself times out (nothing is broadcast) — irrelevant here;
	// the assertion is on what the subscribe sent.
	_ = RunSteps(context.Background(), scope, []Step{step}, cs, WithEventSubscriber(NewSSESubscriber(srv.URL, nil)))
	select {
	case h := <-got:
		if h[0] != "Bearer real-token" {
			t.Errorf("stream Authorization = %q, want the scenario's own token", h[0])
		}
		if h[1] != "text/event-stream" {
			t.Errorf("stream Accept = %q, want text/event-stream", h[1])
		}
	default:
		t.Fatal("subscribe never reached the events route")
	}
}

// TestAwaitEvent_NoSubscriber — a step awaits an event but the run wasn't
// given a subscriber: fail fast with an actionable message.
func TestAwaitEvent_NoSubscriber(t *testing.T) {
	hub := newEventHub()
	srv := hub.server(t, true)
	defer srv.Close()

	steps := []Step{createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events"})}
	cs := map[string]Caller{"rest": NewRESTCaller(srv.URL, nil)}
	err := RunScenario(context.Background(), steps, cs) // no WithEventSubscriber
	if err == nil || !strings.Contains(err.Error(), "no event subscriber configured") {
		t.Fatalf("error = %v, want 'no event subscriber configured'", err)
	}
}
