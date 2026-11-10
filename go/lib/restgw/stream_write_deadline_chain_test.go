package restgw_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	"github.com/wandering-compiler/sdk/go/lib/secheaders"
)

// The write deadline reaches the server's connection through
// http.ResponseController, which unwraps every ResponseWriter wrapper in
// between. The bare-server tests cannot see a wrapper that forgot Unwrap;
// this one runs the stall through the wrappers a generated gateway puts in
// front of a per-RPC stream (security headers, CORS, observability, panic
// recovery), so such a wrapper makes the deadline a silent no-op and fails
// here.
func TestWriteSSEEventWithTimeout_StalledClientEndsTheStreamThroughTheGeneratedChain(t *testing.T) {
	const deadline = 200 * time.Millisecond
	ended := make(chan error, 1)
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	})
	chain := secheaders.Middleware(restgw.CORSMiddleware(restgw.CORSConfig{AllowedOrigins: []string{"https://app.example"}},
		restgw.ObservabilityMiddleware(restgw.RecoverPanicMiddleware(stream), "svc", restgw.RequestIDConfig{})))
	srv := &http.Server{Handler: chain}
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
	if _, err := conn.Write([]byte("GET /stream HTTP/1.1\r\nHost: x\r\nOrigin: https://app.example\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the handler ended without a write error (no flusher through the chain)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("through the generated chain the SSE handler is still writing to a client that stopped reading 10s ago; a wrapper hides the connection from the write deadline")
	}
}
