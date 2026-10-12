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

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// One client that stops reading must cost only its own frames. The hub
// fans out under one lock to every client, so a send that waited on the
// slow client would stall delivery to everybody else.
func TestEventbusEventSource_SlowClientLosesOnlyItsOwnFrames(t *testing.T) {
	const events = 500 // well past the per-client buffer

	bus := eventbus.NewMemoryBus()
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, rawTranscode, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slow, err := source.Subscribe(ctx, []string{"t.x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := source.Subscribe(ctx, []string{"t.x"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Lockstep: the reading client takes each event before the next is
	// sent, so its own buffer never fills and every miss is the hub's.
	for i := 0; i < events; i++ {
		if err := bus.Dispatch(context.Background(), "default", "t.x", wrapperspb.String("e")); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		select {
		case <-fast:
		case <-time.After(5 * time.Second):
			t.Fatalf("the reading client never got event %d of %d — the slow client stalled the hub", i+1, events)
		}
	}

	// The slow client holds a full buffer and nothing more: what did not
	// fit was dropped for it alone.
	if got := len(slow); got != cap(slow) {
		t.Fatalf("slow client has %d queued frames; want its full buffer of %d", got, cap(slow))
	}
}

// A client that keeps its connection open but never reads fills the
// socket buffers, and the next write blocks. The per-write deadline must
// end that write and the handler with it; otherwise the goroutine and its
// subscription live for as long as the peer keeps the connection.
func TestHandleW17Events_StalledClientEndsWithinTheWriteDeadline(t *testing.T) {
	const deadline = 200 * time.Millisecond
	defer restgw.SetW17EventsWriteTimeoutForTest(deadline)()

	bus := eventbus.NewMemoryBus()
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, rawTranscode, nil)

	ended := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, source, time.Hour)
		close(ended)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	if _, err := conn.Write([]byte("GET /w17-events?topics=t.x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// From here on the client reads nothing.

	frame := wrapperspb.String(strings.Repeat("x", 32<<10))
	stop := time.After(10 * time.Second)
	for {
		select {
		case <-ended:
			return
		case <-stop:
			t.Fatal("the handler is still writing to a client that stopped reading 10s ago; the write deadline never fired")
		default:
		}
		_ = bus.Dispatch(context.Background(), "default", "t.x", frame)
		time.Sleep(time.Millisecond)
	}
}
