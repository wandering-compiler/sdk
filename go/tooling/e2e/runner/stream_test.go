package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// streamServer serves ONE server-streaming endpoint over SSE at
// /api/v1/tasks/stream, sending the frames it is given.
//
// `hang` is the reason this is a real server rather than a fake conn: the
// failure a streaming endpoint actually has is sending some frames and then
// neither sending nor closing, and that cannot be expressed by a fake that
// returns from a function.
func streamServer(t *testing.T, frames []string, hang bool, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/stream", func(w http.ResponseWriter, r *http.Request) {
		if status != 0 && status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","message":"stream requires tasks.read"}}`)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("SSE ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		if hang {
			// Hold the connection open until the client gives up, which is
			// exactly what the endpoint under test must not do.
			<-r.Context().Done()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func streamStep(es *ExpectStream) Step {
	return Step{
		Endpoint: Endpoint{
			Ref: "tasks.TasksService.StreamTasks", Transport: "rest", Stream: true,
			HTTPMethod: "GET", PathTemplate: "/api/v1/tasks/stream",
		},
		Label:        "tests/rest/stream_tasks.yaml",
		ExpectStream: es,
	}
}

func runStream(t *testing.T, srv *httptest.Server, s Step) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return RunScenario(ctx, []Step{s},
		map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())},
		WithStreamCaller(NewSSEStreamCaller(srv.URL, srv.Client())))
}

// TestExpectStream_MatchesTheDeclaredSequence — the case the corpus never had:
// a declared streaming endpoint, opened, its frames asserted in order.
func TestExpectStream_MatchesTheDeclaredSequence(t *testing.T) {
	srv := streamServer(t, []string{
		`{"id":"t1","status":"queued"}`,
		`{"id":"t2","status":"running"}`,
		`{"id":"t3","status":"done"}`,
	}, false, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{Frames: []map[string]any{
		{"id": "t1", "status": "queued"},
		{"id": "t2", "status": "running"},
		{"id": "t3", "status": "done"},
	}}))
	if err != nil {
		t.Fatalf("RunScenario: %v", err)
	}
}

// TestExpectStream_OrderIsTheContract — the same three frames, declared in
// the wrong order, must FAIL.
//
// This is the assertion that separates `expect_stream` from `await_events`.
// The bus reader skips to a topic, and skipping is right there; an endpoint
// stream has no siblings to skip past, so a reader that hunted for a match
// would turn "the server sends these in the wrong order" into a pass.
func TestExpectStream_OrderIsTheContract(t *testing.T) {
	srv := streamServer(t, []string{
		`{"id":"t1","status":"queued"}`,
		`{"id":"t2","status":"running"}`,
	}, false, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{Frames: []map[string]any{
		{"id": "t2", "status": "running"},
		{"id": "t1", "status": "queued"},
	}}))
	if err == nil {
		t.Fatal("frames declared in the wrong order passed — order is not being asserted")
	}
	if !strings.Contains(err.Error(), "frame[0]") {
		t.Errorf("the failure does not name which frame disagreed:\n%v", err)
	}
}

// TestExpectStream_AFrameShortIsAFailure — the stream ends early. The
// message has to say so, because "ended after 2" and "went quiet after 2"
// send a reader to different places.
func TestExpectStream_AFrameShortIsAFailure(t *testing.T) {
	srv := streamServer(t, []string{
		`{"id":"t1","status":"queued"}`,
		`{"id":"t2","status":"running"}`,
	}, false, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{Frames: []map[string]any{
		{"id": "t1", "status": "queued"},
		{"id": "t2", "status": "running"},
		{"id": "t3", "status": "done"},
	}}))
	if err == nil {
		t.Fatal("a stream two frames short passed a three-frame contract")
	}
	if !strings.Contains(err.Error(), "ended after 2") {
		t.Errorf("the failure does not distinguish an ended stream from a stalled one:\n%v", err)
	}
}

// TestExpectStream_AServerThatHangsFails — THE trap this feature was
// specified around: "a stream test that asserts 'at least one frame' asserts
// nothing; a lower bound passes for a server that sends one frame and hangs."
//
// Here the server sends every declared frame and then holds the connection
// open forever. Every per-frame assertion is satisfied. The case must still
// fail, because an endpoint that never finishes is one no client can use.
func TestExpectStream_AServerThatHangsFails(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t1","status":"done"}`}, true, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{
		Frames:    []map[string]any{{"id": "t1", "status": "done"}},
		TimeoutMs: 300,
	}))
	if err == nil {
		t.Fatal("a server that sent the declared frame and then hung passed — the close is half the contract")
	}
	if !strings.Contains(err.Error(), "neither sent another nor closed") {
		t.Errorf("the failure does not name what went wrong:\n%v", err)
	}
}

// TestExpectStream_AllowMoreIsTheOptOut — an endless feed says so in the
// file, and then the same server passes. The point is that the lower bound
// is reinstated DELIBERATELY and visibly, never by default.
func TestExpectStream_AllowMoreIsTheOptOut(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t1","status":"done"}`}, true, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{
		Frames:    []map[string]any{{"id": "t1", "status": "done"}},
		AllowMore: true,
		TimeoutMs: 300,
	}))
	if err != nil {
		t.Fatalf("allow_more should accept a feed that keeps going: %v", err)
	}
}

// TestExpectStream_ExtraFrameFails — the other end of the same contract: the
// server sends one MORE than declared. Without this, `frames:` would be a
// prefix assertion, which is the lower bound wearing a different hat.
func TestExpectStream_ExtraFrameFails(t *testing.T) {
	srv := streamServer(t, []string{
		`{"id":"t1","status":"queued"}`,
		`{"id":"t2","status":"running"}`,
	}, false, 0)

	err := runStream(t, srv, streamStep(&ExpectStream{
		Frames:    []map[string]any{{"id": "t1", "status": "queued"}},
		TimeoutMs: 500,
	}))
	if err == nil {
		t.Fatal("an extra frame passed — `frames` would be a prefix, not a contract")
	}
	if !strings.Contains(err.Error(), "sent a frame after") {
		t.Errorf("the failure does not name the extra frame:\n%v", err)
	}
}

// TestExpectStream_RefusedConnectIsAnOrdinaryRefusal — the second trap: "it
// needs a refusable case too". A caller with no permission never gets a
// stream at all; the connect answers the same REST envelope a refused unary
// call does, so the case is written with the `expect_error:` vocabulary
// every other refusal already uses.
func TestExpectStream_RefusedConnectIsAnOrdinaryRefusal(t *testing.T) {
	srv := streamServer(t, nil, false, http.StatusForbidden)

	s := streamStep(nil)
	s.ExpectError = &ExpectError{
		Code:    "PERMISSION_DENIED",
		Message: map[string]any{"matcher": "regex", "pattern": "tasks.read"},
	}
	if err := runStream(t, srv, s); err != nil {
		t.Fatalf("a refused stream connect should satisfy expect_error: %v", err)
	}
}

// TestExpectStream_RefusalCaseFailsWhenTheStreamOPENS — the decoy for the
// test above. A refusal assertion that passes when nothing refuses is the
// failure mode `expect_error` exists to prevent, one transport down.
func TestExpectStream_RefusalCaseFailsWhenTheStreamOPENS(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t1"}`}, false, 0)

	s := streamStep(nil)
	s.ExpectError = &ExpectError{Code: "PERMISSION_DENIED"}
	if err := runStream(t, srv, s); err == nil {
		t.Fatal("an endpoint that opened the stream satisfied a PERMISSION_DENIED assertion")
	}
}

// TestExpectStream_NoStreamCallerIsLoud — a step that declares
// `expect_stream` without a caller wired must FAIL, not skip. A stream case
// that quietly asserted nothing would be worse than the gap it replaced,
// which was at least visible.
func TestExpectStream_NoStreamCallerIsLoud(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t1"}`}, false, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RunScenario(ctx, []Step{streamStep(&ExpectStream{Frames: []map[string]any{{"id": "t1"}}})},
		map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())})
	if err == nil || !strings.Contains(err.Error(), "WithStreamCaller") {
		t.Fatalf("a missing stream caller must name itself, got: %v", err)
	}
}

// TestExpectStream_CapturesBindFromFrames — a frame's matcher vocabulary is
// the one `expect:` has, captures included, so a later step can use an id
// the stream carried.
func TestExpectStream_CapturesBindFromFrames(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t-42","status":"done"}`}, false, 0)

	scope := runtime.NewRun().NewScope()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunSteps(ctx, scope, []Step{streamStep(&ExpectStream{Frames: []map[string]any{
		{"id": map[string]any{"capture": "task.id"}, "status": "done"},
	}})}, map[string]Caller{"rest": NewRESTCaller(srv.URL, srv.Client())},
		WithStreamCaller(NewSSEStreamCaller(srv.URL, srv.Client())))
	if err != nil {
		t.Fatalf("RunSteps: %v", err)
	}
	got, ok := scope.Get("task.id")
	if !ok || fmt.Sprint(got) != "t-42" {
		t.Errorf("capture from a frame = %v (ok=%v), want t-42", got, ok)
	}
}

// TestSSEStreamCaller_ClosedStreamIsNotATimeout — the distinction the whole
// file rests on, asserted at the seam that produces it.
func TestSSEStreamCaller_ClosedStreamIsNotATimeout(t *testing.T) {
	srv := streamServer(t, []string{`{"id":"t1"}`}, false, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := NewSSEStreamCaller(srv.URL, srv.Client()).OpenStream(ctx,
		Endpoint{Ref: "x", HTTPMethod: "GET", PathTemplate: "/api/v1/tasks/stream", Stream: true},
		nil, "", nil)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Next(ctx, time.Second); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if _, err := conn.Next(ctx, time.Second); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("a finished stream reported %v, want ErrStreamClosed — a timeout here would read as a hung server", err)
	}
}
