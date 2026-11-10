package restgw_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// stallingConn dials with a small receive buffer: a client that then reads
// nothing fills it and the server's socket send buffer within a few writes.
func stallingDial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err == nil {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4 << 10)
		}
	}
	return c, err
}

// A per-RPC SSE stream to a client that stopped reading MUST end within the
// write deadline: WriteSSEEventWithTimeout returns an error once a write
// cannot finish in time, and the handler — the goroutine a half-open client
// would otherwise pin for good — returns. Without the deadline the write
// blocks until the TCP stack gives up, minutes later.
func TestWriteSSEEventWithTimeout_StalledClientEndsTheStream(t *testing.T) {
	const deadline = 200 * time.Millisecond
	ended := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := restgw.WriteSSEHeaders(w)
		if !ok {
			ended <- nil
			return
		}
		frame := wrapperspb.String(strings.Repeat("x", 32<<10))
		for {
			if err := restgw.WriteSSEEventWithTimeout(w, flusher, frame, deadline); err != nil {
				ended <- err
				return
			}
		}
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	conn, err := stallingDial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /stream HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// From here on the client reads nothing.
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the handler ended without a write error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the SSE handler is still writing to a client that stopped reading 10s ago; the write deadline never fired")
	}
}

// The same for a WebSocket stream: WSWriteProtoWithTimeout MUST give up on a
// client that stopped reading within the timeout.
func TestWSWriteProtoWithTimeout_StalledClientEndsTheStream(t *testing.T) {
	const deadline = 200 * time.Millisecond
	ended := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := restgw.AcceptWebSocket(w, r)
		if err != nil {
			ended <- nil
			return
		}
		defer func() { _ = conn.CloseNow() }()
		frame := wrapperspb.String(strings.Repeat("x", 32<<10))
		for {
			if err := restgw.WSWriteProtoWithTimeout(r.Context(), conn, frame, deadline); err != nil {
				ended <- err
				return
			}
		}
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	client := &http.Client{Transport: &http.Transport{DialContext: stallingDial}}
	c, _, err := websocket.Dial(context.Background(), "ws://"+ln.Addr().String()+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	// The client never reads.
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the handler ended without a write error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the WebSocket handler is still writing to a client that stopped reading 10s ago; the write timeout never fired")
	}
}
