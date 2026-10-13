package restgw

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
)

// serveBound runs h on a loopback server bound to the drain plumbing and
// returns the stopper and the address.
func serveBound(t *testing.T, h http.HandlerFunc) (*lifecycle.HTTPStopper, string) {
	t.Helper()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	stopper := lifecycle.BindHTTP(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return stopper, ln.Addr().String()
}

// A per-RPC SSE stream does not watch the drain signal, like the generated
// relay of a server-streaming RPC: it ends when the stop bound runs out,
// by the cancelled request context, and the stop is not reported as a
// failure. The client sees its connection end.
func TestPerRPCSSEStream_EndsAtTheStopBound(t *testing.T) {
	ended := make(chan error, 1)
	stopper, addr := serveBound(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := WriteSSEHeaders(w)
		if !ok {
			t.Error("no flusher")
			return
		}
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				ended <- r.Context().Err()
				return
			case <-tick.C:
				if err := WriteSSEEvent(w, flusher, wrapperspb.String("frame")); err != nil {
					ended <- err
					return
				}
			}
		}
	})
	resp, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewScanner(resp.Body)
	if !lines.Scan() {
		t.Fatalf("no first frame: %v", lines.Err())
	}
	clientDone := make(chan struct{})
	go func() {
		for lines.Scan() {
		}
		close(clientDone)
	}()

	start := time.Now()
	if err := stopper.Stop(500 * time.Millisecond); err != nil {
		t.Fatalf("Stop = %v; a stream outliving the bound is not a failed stop", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Stop took %v with a per-RPC stream open", took)
	}
	select {
	case err := <-ended:
		if err != context.Canceled {
			t.Errorf("the stream ended with %v, want its request context cancelled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the per-RPC stream handler outlived the stop")
	}
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the client's stream did not end")
	}
}

// A per-RPC WebSocket is a hijacked connection: the HTTP server neither
// waits for it nor closes it. It still ends when the server stops, because
// the stop cancels every request context and the socket is driven by it.
func TestPerRPCWebSocketStream_EndsWhenTheServerStops(t *testing.T) {
	ended := make(chan struct{})
	stopper, addr := serveBound(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		conn, err := AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			if err := conn.Write(r.Context(), websocket.MessageBinary, []byte("frame")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+addr+"/stream", &websocket.DialOptions{Subprotocols: []string{wsSubprotocolPB}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("no first frame: %v", err)
	}
	clientDone := make(chan struct{})
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				close(clientDone)
				return
			}
		}
	}()

	if err := stopper.Stop(10 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the WebSocket handler outlived the stop")
	}
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the client's socket did not end")
	}
}
