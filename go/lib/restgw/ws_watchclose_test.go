package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// wsWriteOnlyResult is what the write-only handler below reports back to
// the test: whether the context the backend stream would run on was
// cancelled, and how many frames it managed to write in the meantime.
type wsWriteOnlyResult struct {
	cancelled     bool
	writesAfterUp int
	writeErr      error
}

// TestWSWatchClientClose_GracefulCloseReleasesTheStream pins the invariant
// C-F5 filed: a WRITE-ONLY WebSocket handler — the shape the generated
// server-stream-over-WS branch has — must notice a client that closes
// GRACEFULLY and cancel the context its backend gRPC stream runs on.
//
// Why the invariant is stated as "the ctx is cancelled" rather than "the
// write fails": a graceful close leaves the TCP connection up (the peer is
// in CLOSING state, exactly what a browser does on unmount), so writes keep
// succeeding at the transport level and the 30s per-frame write deadline
// never fires. Nothing else reaps it either — the hijacked request context
// has no deadline, gRPC keepalive reaps dead TRANSPORTS not live streams,
// and the shutdown drain only bounds streams at process exit. So without an
// active reader the handler goroutine, the socket and the upstream gRPC
// stream live until the process dies, once per closed client.
//
// The bound is 2s and the client's close-handshake timeout is 5s, so the
// assertion is decided while the client is still parked on the handshake —
// the failure can never be "the client eventually dropped the TCP".
func TestWSWatchClientClose_GracefulCloseReleasesTheStream(t *testing.T) {
	const (
		observeFor = 2 * time.Second
		writeEvery = 50 * time.Millisecond
	)

	results := make(chan wsWriteOnlyResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		// The emitted branch, verbatim in shape: a cancellable child of the
		// hijacked request ctx, the close watcher, then WRITES ONLY.
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		ctx = restgw.WSWatchClientClose(ctx, conn)

		res := wsWriteOnlyResult{}
		deadline := time.After(observeFor)
		tick := time.NewTicker(writeEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				res.cancelled = true
				results <- res
				return
			case <-deadline:
				results <- res
				return
			case <-tick.C:
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"w17":"d","data":{}}`)); err != nil {
					res.writeErr = err
					results <- res
					return
				}
				res.writesAfterUp++
			}
		}
	}))
	defer srv.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	c, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	// The graceful close every browser performs: send the close frame and
	// wait for the peer's echo. Off the test goroutine, because a server
	// that never reads never echoes and the client would park here for its
	// full 5s handshake timeout — which is the defect, not the assertion.
	go func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	select {
	case res := <-results:
		if !res.cancelled {
			t.Fatalf("the client completed the WebSocket close handshake, but after %s the handler had "+
				"not noticed: the backend stream's context was never cancelled and %d further frames were "+
				"written successfully (writeErr=%v). One goroutine, one socket and one upstream gRPC stream "+
				"leak per closed client.", observeFor, res.writesAfterUp, res.writeErr)
		}
	case <-time.After(observeFor + 5*time.Second):
		t.Fatal("handler never reported")
	}
}

// TestWSWatchClientClose_HealthyStreamKeepsRunning is the other half of the
// pin, and the reason the fix cannot simply be "cancel on the first read":
// a healthy client that sends nothing (the normal server-stream case — the
// request rode in on the URL) must NOT be torn down. A watcher that
// mistook silence, or its own read call returning, for a disconnect would
// kill every working subscription.
func TestWSWatchClientClose_HealthyStreamKeepsRunning(t *testing.T) {
	const observeFor = 750 * time.Millisecond

	results := make(chan wsWriteOnlyResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		ctx = restgw.WSWatchClientClose(ctx, conn)

		res := wsWriteOnlyResult{}
		deadline := time.After(observeFor)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				res.cancelled = true
				results <- res
				return
			case <-deadline:
				results <- res
				return
			case <-tick.C:
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"w17":"d","data":{}}`)); err != nil {
					res.writeErr = err
					results <- res
					return
				}
				res.writesAfterUp++
			}
		}
	}))
	defer srv.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	c, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	// A real consumer: read the frames as they arrive, never send.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := c.Read(dialCtx); err != nil {
				return
			}
		}
	}()

	select {
	case res := <-results:
		if res.cancelled {
			t.Fatalf("a healthy, silent client was torn down: the close watcher cancelled the backend "+
				"stream after %d frames (writeErr=%v)", res.writesAfterUp, res.writeErr)
		}
		if res.writeErr != nil {
			t.Fatalf("a healthy client's writes failed: %v", res.writeErr)
		}
		if res.writesAfterUp == 0 {
			t.Fatalf("no frames were delivered to a healthy client — the probe proved nothing")
		}
	case <-time.After(observeFor + 5*time.Second):
		t.Fatal("handler never reported")
	}
	_ = c.CloseNow()
	<-readDone
}
