package lifecycle_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
)

// --- gRPC ------------------------------------------------------------

// blockingGRPCServer starts a real gRPC server whose (unknown-service)
// handler blocks until release is closed — a stand-in for the long codegen
// call / held stream that GracefulStop would wait on forever. It returns
// the server, the address, and a channel closed once a call is actually
// in flight.
func blockingGRPCServer(t *testing.T, release <-chan struct{}) (*grpc.Server, string, <-chan struct{}) {
	t.Helper()
	inFlight := make(chan struct{})
	var once sync.Once
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		once.Do(func() { close(inFlight) })
		select {
		case <-release:
		case <-stream.Context().Done():
		}
		return nil
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Stop() })
	return srv, ln.Addr().String(), inFlight
}

// dialAndHold opens a stream against the blocking handler and leaves it
// open. The returned cancel tears the client side down.
func dialAndHold(t *testing.T, addr string) context.CancelFunc {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/held.Service/Hold")
	if err != nil {
		cancel()
		_ = conn.Close()
		t.Fatal(err)
	}
	return func() { cancel(); _ = conn.Close() }
}

// A held RPC must not outlive the timeout: the point of A-2. Without the
// fallback this test hangs until the go-test deadline.
func TestStopGRPCForcesHardStopOnHeldRPC(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, addr, inFlight := blockingGRPCServer(t, release)
	cancelClient := dialAndHold(t, addr)
	defer cancelClient()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		lifecycle.StopGRPC(srv, 500*time.Millisecond)
		done <- time.Since(start)
	}()
	select {
	case took := <-done:
		if took < 400*time.Millisecond {
			t.Fatalf("stopped in %s — the graceful window was skipped, not honoured", took)
		}
		if took > 3*time.Second {
			t.Fatalf("stopped in %s — the timeout did not bound the wait", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopGRPC never returned — no timeout fallback (A-2)")
	}
}

// The other half: a server with nothing in flight must stop AT ONCE. A
// fallback that always waits out the timeout adds it to every deploy.
func TestStopGRPCReturnsImmediatelyWhenIdle(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _, _ := blockingGRPCServer(t, release)

	start := time.Now()
	lifecycle.StopGRPC(srv, 10*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("idle stop took %s — the graceful path is waiting out the timeout", took)
	}
}

// --- HTTP ------------------------------------------------------------

// sseServer stands up an http.Server whose handler mimics a live SSE
// stream: headers out, then park until told to end. drainAware picks
// whether it watches the drain signal (the reserved w17-events shape) or
// only its request context (a handler that ignores shutdown entirely).
func sseServer(t *testing.T, drainAware bool) (*lifecycle.HTTPStopper, string, func()) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		var drain <-chan struct{}
		if drainAware {
			drain = lifecycle.Draining(r.Context())
		}
		select {
		case <-r.Context().Done():
		case <-drain:
		}
	})}
	stopper := lifecycle.BindHTTP(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return stopper, ln.Addr().String(), func() { _ = srv.Close() }
}

// holdSSE opens a raw HTTP connection, waits for the response line, and
// leaves the body unread — a client parked on a stream.
func holdSSE(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET /w17-events HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A drain-aware stream must end AS SHUTDOWN STARTS — not be waited out.
// This is the A-1 symptom: +10s and a false error on every deploy that
// had an SSE client attached.
func TestStopHTTPDrainAwareStreamEndsImmediately(t *testing.T) {
	stopper, addr, cleanup := sseServer(t, true)
	defer cleanup()
	conn := holdSSE(t, addr)
	defer func() { _ = conn.Close() }()

	start := time.Now()
	err := stopper.Stop(10 * time.Second)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Stop returned %v — a clean drain must not fail the process", err)
	}
	if took > 2*time.Second {
		t.Fatalf("drained in %s — the stream was waited out instead of told to end", took)
	}
}

// A handler that ignores the drain signal still must not outlive the
// timeout, and its overrun must NOT be reported as a process error.
func TestStopHTTPForcesCloseAndReportsNoError(t *testing.T) {
	stopper, addr, cleanup := sseServer(t, false)
	defer cleanup()
	conn := holdSSE(t, addr)
	defer func() { _ = conn.Close() }()

	start := time.Now()
	err := stopper.Stop(500 * time.Millisecond)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("forced close reported %v — a bounded stop is not a failed deploy", err)
	}
	if took > 3*time.Second {
		t.Fatalf("Stop took %s — the timeout did not bound the wait", took)
	}
}

// Nothing in flight → immediate, exactly as before the fix.
func TestStopHTTPIdleReturnsImmediately(t *testing.T) {
	stopper, _, cleanup := sseServer(t, true)
	defer cleanup()
	start := time.Now()
	if err := stopper.Stop(10 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("idle stop took %s", took)
	}
}

// A handler blocked on a backend call sees its context cancelled by the
// forced path, so it unwinds through its own error path instead of only
// noticing when the socket dies.
func TestStopHTTPForcedPathCancelsRequestContexts(t *testing.T) {
	unwound := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		unwound <- r.Context().Err()
	})}
	stopper := lifecycle.BindHTTP(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	conn := holdSSE(t, ln.Addr().String())
	defer func() { _ = conn.Close() }()

	if err := stopper.Stop(300 * time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case cerr := <-unwound:
		if !errors.Is(cerr, context.Canceled) {
			t.Fatalf("request context ended with %v, want context.Canceled", cerr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was never cancelled by the forced path")
	}
}

// Draining is safe on a context that carries no signal — a handler
// running outside a bound server must not panic or spin.
func TestDrainingAbsentIsNilChannel(t *testing.T) {
	if ch := lifecycle.Draining(context.Background()); ch != nil {
		t.Fatalf("Draining on a bare context returned %v, want nil", ch)
	}
}

func TestShutdownTimeoutFromEnv(t *testing.T) {
	env := map[string]string{}
	lookup := func(k string) string { return env[k] }

	if got := lifecycle.ShutdownTimeoutFromEnv("APP", lookup); got != lifecycle.DefaultShutdownTimeout {
		t.Fatalf("unset → %s, want %s", got, lifecycle.DefaultShutdownTimeout)
	}
	env["APP_SHUTDOWN_TIMEOUT_SECONDS"] = "25"
	if got := lifecycle.ShutdownTimeoutFromEnv("APP", lookup); got != 25*time.Second {
		t.Fatalf("25 → %s", got)
	}
	env["APP_SHUTDOWN_TIMEOUT_SECONDS"] = "nonsense"
	if got := lifecycle.ShutdownTimeoutFromEnv("APP", lookup); got != lifecycle.DefaultShutdownTimeout {
		t.Fatalf("unparseable → %s, want the default", got)
	}
	env["APP_SHUTDOWN_TIMEOUT_SECONDS"] = "0"
	if got := lifecycle.ShutdownTimeoutFromEnv("APP", lookup); got != 0 {
		t.Fatalf("explicit 0 → %s, want the unbounded opt-out", got)
	}
}
