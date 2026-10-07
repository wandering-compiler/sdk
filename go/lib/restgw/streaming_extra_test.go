package restgw_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// WriteSSEGRPCErrorCtx carries the envelope's details[] — the SSE leg of the
// B25-restgw-1 parity the unary and WS writers already had. On a stream this
// frame is the ONLY place a backend refusal can go (the 200 is already sent),
// and it was dropping the stable code a client dispatches on: a consumer's
// overload refusal arrived as bare RESOURCE_EXHAUSTED with PARSE_OVERLOADED
// nowhere on the wire (2026-10-06).
func TestWriteSSEGRPCErrorCtx_ForwardsDetails(t *testing.T) {
	st, err := status.New(codes.ResourceExhausted, "ParseService.Stream: queue full").WithDetails(
		protoadapt.MessageV1Of(&w17pb.ErrorDetail{Code: "PARSE_OVERLOADED", Message: "Busy, try again."}),
	)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	rec := httptest.NewRecorder()
	flusher, _ := restgw.WriteSSEHeaders(rec)
	restgw.WriteSSEGRPCErrorCtxWithTimeout(context.Background(), rec, flusher, st.Err(), time.Second)

	body := rec.Body.String()
	data, ok := strings.CutPrefix(strings.TrimSpace(body), "event: error\ndata: ")
	if !ok {
		t.Fatalf("not one SSE error frame: %q", body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		t.Fatalf("frame payload is not the envelope: %v\n%s", err, data)
	}
	if env.Error.Code != "RESOURCE_EXHAUSTED" || env.Error.Message != "Busy, try again." {
		t.Errorf("code/message = %q/%q, want RESOURCE_EXHAUSTED and the detail's sentence", env.Error.Code, env.Error.Message)
	}
	if len(env.Error.Details) != 1 || env.Error.Details[0].Code != "PARSE_OVERLOADED" {
		t.Errorf("details = %+v, want the backend's PARSE_OVERLOADED", env.Error.Details)
	}
	if strings.Contains(body, "queue full") {
		t.Errorf("the developer's message rode out on the stream: %q", body)
	}
}

// WSWriteGRPCError forwards a backend status's *w17.ErrorDetail field
// violations into the WS error envelope (B25-restgw-1 parity with the
// unary path) — the details-bearing branch, distinct from the bare
// status path already covered.
func TestWSWriteGRPCError_ForwardsFieldViolations(t *testing.T) {
	st, err := status.New(codes.AlreadyExists, "email already in use").WithDetails(
		protoadapt.MessageV1Of(&w17pb.ErrorDetail{Field: "email", Code: "UNIQUE_VIOLATION", Message: "already taken"}),
	)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	wsRoundTrip(t, nil,
		func(ctx context.Context, conn *websocket.Conn) {
			restgw.WSWriteGRPCError(ctx, conn, st.Err())
		},
		func(ctx context.Context, conn *websocket.Conn) {
			_, data, rerr := conn.Read(ctx)
			if rerr != nil {
				t.Fatalf("read: %v", rerr)
			}
			body := string(data)
			if !strings.Contains(body, "ALREADY_EXISTS") {
				t.Errorf("envelope should carry the canonical code; got %s", body)
			}
			if !strings.Contains(body, "email") || !strings.Contains(body, "UNIQUE_VIOLATION") {
				t.Errorf("envelope should carry the field violation details; got %s", body)
			}
		})
}

// WriteSSEGRPCErrorWithTimeout installs + clears a write deadline on a
// writer that supports SetWriteDeadline (a real TCP conn), then emits
// the terminal error event — the timeout>0 arm.
func TestWriteSSEGRPCErrorWithTimeout_DeadlineInstalled(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := restgw.WriteSSEHeaders(w)
		if !ok {
			return
		}
		// Real http.ResponseWriter over TCP supports SetWriteDeadline, so
		// the timeout>0 deadline arm runs (and the defer clears it).
		restgw.WriteSSEGRPCErrorWithTimeout(w, flusher,
			status.Error(codes.FailedPrecondition, "nope"), 2*time.Second)
		got <- "done"
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "FAILED_PRECONDITION") {
		t.Errorf("error event should carry the mapped code; got %s", body)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Error("handler never completed")
	}
}
