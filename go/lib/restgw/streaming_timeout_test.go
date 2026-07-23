package restgw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
)

// G3-GW-09: bounded-write helpers. Real back-pressure
// behaviour needs a slow-client TCP scenario which is
// fiddly to drive in unit tests; the unit cases here cover
// the helper contract — `timeout=0` is a no-op pass-through,
// timeout > 0 installs and clears the deadline / derived
// ctx. Production back-pressure is verified manually with
// `slowloris`-style HTTP probes.

func TestWriteSSEEventWithTimeout_ZeroTimeoutPassthrough(t *testing.T) {
	// `timeout=0` matches WriteSSEEvent verbatim — no
	// deadline installation, no behavioural divergence.
	rr := httptest.NewRecorder()
	flusher, ok := restgw.WriteSSEHeaders(rr)
	if !ok {
		t.Fatal("httptest.NewRecorder should implement http.Flusher")
	}
	msg := &distxpb.BeginRequest{ConnectionName: "main"}
	if err := restgw.WriteSSEEventWithTimeout(rr, flusher, msg, 0); err != nil {
		t.Fatalf("WriteSSEEventWithTimeout: %v", err)
	}
	body := rr.Body.String()
	if body == "" {
		t.Error("expected SSE event in body")
	}
	if !contains2(body, "data:") {
		t.Errorf("missing data prefix:\n%s", body)
	}
}

func TestWriteSSEEventWithTimeout_RecorderFallsBackToNoDeadline(t *testing.T) {
	// httptest.ResponseRecorder doesn't implement
	// SetWriteDeadline (it's a memory buffer); the
	// helper falls back to the no-deadline path so the
	// write still works against tests + reverse-proxy
	// wrappers that don't expose Hijacker / Deadliner.
	rr := httptest.NewRecorder()
	flusher, _ := restgw.WriteSSEHeaders(rr)
	msg := &distxpb.BeginRequest{ConnectionName: "main"}
	if err := restgw.WriteSSEEventWithTimeout(rr, flusher, msg, 5*time.Second); err != nil {
		t.Fatalf("WriteSSEEventWithTimeout (with deadline): %v", err)
	}
	if rr.Body.String() == "" {
		t.Error("expected SSE body even when deadline silently fails to install")
	}
}

func TestWSWriteProtoWithTimeout_ZeroTimeoutPassthrough(t *testing.T) {
	// `timeout=0` short-circuits to WSWriteProto —
	// no derived ctx, no allocation overhead. Wire up a
	// pair of in-memory WS conns + verify the message
	// round-trips.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		_ = restgw.WSWriteProtoWithTimeout(r.Context(), conn,
			&distxpb.BeginRequest{ConnectionName: "main"}, 0, restgw.WireJSON)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	got := &distxpb.BeginRequest{}
	if err := restgw.WSReadProto(ctx, conn, got, restgw.WireJSON); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	if got.GetConnectionName() != "main" {
		t.Errorf("expected ConnectionName=main; got %q", got.GetConnectionName())
	}
}

func TestWSWriteProtoWithTimeout_PassesTimeoutThrough(t *testing.T) {
	// timeout > 0 derives a child ctx with the deadline
	// installed. Verify the helper doesn't blow up on
	// happy-path writes when a non-zero timeout is set
	// (timeout much larger than the actual write
	// latency). The ctx-cancellation propagation
	// behaviour is coder/websocket's contract — we don't
	// re-test it here, just confirm the wrapper composes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		_ = restgw.WSWriteProtoWithTimeout(r.Context(), conn,
			&distxpb.BeginRequest{ConnectionName: "audit"}, 5*time.Second, restgw.WireJSON)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	got := &distxpb.BeginRequest{}
	if err := restgw.WSReadProto(ctx, conn, got, restgw.WireJSON); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	if got.GetConnectionName() != "audit" {
		t.Errorf("expected ConnectionName=audit; got %q", got.GetConnectionName())
	}
}

// Q5-gw-1: error-path bounded-write helpers. Same contract as
// the G3-GW-09 main-frame helpers above — `timeout=0` is a
// verbatim pass-through, `timeout>0` installs/derives the
// deadline without altering the emitted error frame. Each case
// asserts the WithTimeout helper produces byte-identical output
// to its base counterpart so the terminal error envelope on the
// wire is unchanged.

func TestWriteSSEGRPCErrorWithTimeout_MatchesBase(t *testing.T) {
	err := status.Error(codes.NotFound, "missing widget")

	base := httptest.NewRecorder()
	bf, _ := restgw.WriteSSEHeaders(base)
	restgw.WriteSSEGRPCError(base, bf, err)

	// timeout > 0 — the recorder can't install SetWriteDeadline
	// (memory buffer), so the helper falls back to the no-deadline
	// path; the emitted error event must stay byte-identical.
	withTO := httptest.NewRecorder()
	wf, _ := restgw.WriteSSEHeaders(withTO)
	restgw.WriteSSEGRPCErrorWithTimeout(withTO, wf, err, 5*time.Second)

	// timeout == 0 — explicit pass-through to WriteSSEGRPCError.
	zero := httptest.NewRecorder()
	zf, _ := restgw.WriteSSEHeaders(zero)
	restgw.WriteSSEGRPCErrorWithTimeout(zero, zf, err, 0)

	if withTO.Body.String() != base.Body.String() {
		t.Errorf("timeout>0 body diverged from base:\n base=%q\n  got=%q", base.Body.String(), withTO.Body.String())
	}
	if zero.Body.String() != base.Body.String() {
		t.Errorf("timeout==0 body diverged from base:\n base=%q\n  got=%q", base.Body.String(), zero.Body.String())
	}
	if !contains2(base.Body.String(), "NOT_FOUND") {
		t.Errorf("expected NOT_FOUND code in SSE error event:\n%s", base.Body.String())
	}
}

// wsReadOneFrame spins up a WS server that invokes `write` against
// the accepted conn, dials it, and returns the first frame the
// client reads. Used to capture the exact bytes an error helper
// puts on the wire before it closes the conn.
func wsReadOneFrame(t *testing.T, write func(ctx context.Context, conn *websocket.Conn)) []byte {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		write(r.Context(), conn)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read frame: %v", err)
	}
	return data
}

func TestWSWriteGRPCErrorWithTimeout_MatchesBase(t *testing.T) {
	err := status.Error(codes.PermissionDenied, "nope")

	base := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteGRPCError(ctx, conn, err)
	})
	withTO := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteGRPCErrorWithTimeout(ctx, conn, err, 5*time.Second)
	})
	zero := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteGRPCErrorWithTimeout(ctx, conn, err, 0)
	})

	if string(withTO) != string(base) {
		t.Errorf("timeout>0 frame diverged from base:\n base=%q\n  got=%q", base, withTO)
	}
	if string(zero) != string(base) {
		t.Errorf("timeout==0 frame diverged from base:\n base=%q\n  got=%q", base, zero)
	}
	if !contains2(string(base), "PERMISSION_DENIED") {
		t.Errorf("expected PERMISSION_DENIED in WS error frame:\n%s", base)
	}
}

func TestWSWriteErrorWithDetailsWithTimeout_MatchesBase(t *testing.T) {
	details := []restgw.FieldError{{Field: "name", Message: "required"}}

	base := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteErrorWithDetails(ctx, conn, "INVALID_ARGUMENT", "validation failed", details)
	})
	withTO := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteErrorWithDetailsWithTimeout(ctx, conn, "INVALID_ARGUMENT", "validation failed", details, 5*time.Second)
	})
	zero := wsReadOneFrame(t, func(ctx context.Context, conn *websocket.Conn) {
		restgw.WSWriteErrorWithDetailsWithTimeout(ctx, conn, "INVALID_ARGUMENT", "validation failed", details, 0)
	})

	if string(withTO) != string(base) {
		t.Errorf("timeout>0 frame diverged from base:\n base=%q\n  got=%q", base, withTO)
	}
	if string(zero) != string(base) {
		t.Errorf("timeout==0 frame diverged from base:\n base=%q\n  got=%q", base, zero)
	}
	if !contains2(string(base), "validation failed") {
		t.Errorf("expected validation-failed message in WS error frame:\n%s", base)
	}
}

func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
