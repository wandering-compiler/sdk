package restgw_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// openSource is an EventSource whose channel stays open until the
// subscription context is cancelled — i.e. the normal state of every
// browser parked on /w17-events with nothing to read yet.
type openSource struct{}

func (openSource) Subscribe(ctx context.Context, _ []string, _ []byte) (<-chan restgw.Event, error) {
	ch := make(chan restgw.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// eventsServer stands up the reserved SSE channel behind a bound server.
func eventsServer(t *testing.T) (*lifecycle.HTTPStopper, string, func()) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, openSource{}, time.Hour)
	})}
	stopper := lifecycle.BindHTTP(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return stopper, ln.Addr().String(), func() { _ = srv.Close() }
}

// holdEvents opens a raw connection to the SSE route and reads the status
// line, so the handler is provably inside its select loop, then leaves the
// body unread — a live client.
func holdEvents(t *testing.T, addr string) net.Conn {
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

// A connected SSE client must not delay shutdown. Before the drain signal
// this cost the full graceful window on every deploy AND surfaced as
// context.DeadlineExceeded — a red deploy for a stream doing nothing.
func TestW17EventsEndsOnDrain(t *testing.T) {
	stopper, addr, cleanup := eventsServer(t)
	defer cleanup()
	conn := holdEvents(t, addr)
	defer func() { _ = conn.Close() }()

	start := time.Now()
	err := stopper.Stop(10 * time.Second)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Stop reported %v — a parked SSE client is not a failed shutdown", err)
	}
	if took > 2*time.Second {
		t.Fatalf("shutdown took %s with one idle SSE client — it was waited out, not drained", took)
	}
}

// The handler still exits on plain client disconnect (the pre-existing
// contract): the drain arm must not have replaced the ctx arm.
func TestW17EventsStillEndsOnClientDisconnect(t *testing.T) {
	done := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, openSource{}, time.Hour)
		close(done)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	conn := holdEvents(t, ln.Addr().String())
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}
}
