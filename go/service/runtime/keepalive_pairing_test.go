package runtime

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// The client half of the keepalive pairing against the server's real
// enforcement: an idle connection (no stream open) pinged at an interval
// no longer than the w17 client's keepalive Time is never answered with
// GOAWAY "too_many_pings". The server counts a strike for each ping that
// arrives sooner than its MinTime and closes the connection after the
// third, so four pings at that interval are enough to see a violation.
//
// The client is raw HTTP/2, as in the reap test, so the ping interval is
// the test's and not the gRPC client's (whose shortest Time is 10s). The
// interval sits between the server's MinTime and the client's Time: a
// server that permits it permits the client's slower pings.
func TestGRPCComponent_PermitsTheClientsKeepalivePingsOnAnIdleConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("33s ping cycle")
	}
	const interval = 11 * time.Second
	if kp := grpcclient.DefaultClientKeepalive; kp.Time < interval || !kp.PermitWithoutStream {
		t.Fatalf("client keepalive %+v pings faster than %v or only with a stream open; this test no longer covers it", kp, interval)
	}
	noTLS(t)
	addr := freeAddr(t)
	stop := runComponent(t, Config{Addr: addr, RegisterServices: func(*grpc.Server) error { return nil }})
	defer func() { _ = stop() }()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, c, 0x4, 0, nil) // our SETTINGS, empty

	// Read frames in the background: ack the server's SETTINGS and pings,
	// count our ping acks, and report a GOAWAY or a closed connection.
	acks := make(chan struct{}, 8)
	ended := make(chan string, 1)
	go func() {
		for {
			typ, flags, payload, err := readFramePayload(c)
			if err != nil {
				ended <- "connection closed: " + err.Error()
				return
			}
			switch {
			case typ == 0x4 && flags&0x1 == 0:
				writeFrame(t, c, 0x4, 0x1, nil)
			case typ == 0x6 && flags&0x1 != 0:
				acks <- struct{}{}
			case typ == 0x6:
				writeFrame(t, c, 0x6, 0x1, payload)
			case typ == 0x7:
				ended <- "GOAWAY " + string(bytes.TrimLeft(payload[min(8, len(payload)):], "\x00"))
				return
			}
		}
	}()

	ping := make([]byte, 8)
	for i := range 4 {
		if i > 0 {
			select {
			case why := <-ended:
				t.Fatalf("ping %d: the server ended an idle connection pinged every %v: %s", i, interval, why)
			case <-time.After(interval):
			}
		}
		ping[7] = byte(i + 1)
		writeFrame(t, c, 0x6, 0, ping)
		select {
		case <-acks:
		case why := <-ended:
			t.Fatalf("ping %d: the server ended an idle connection pinged every %v: %s", i, interval, why)
		case <-time.After(5 * time.Second):
			t.Fatalf("ping %d was not acknowledged", i)
		}
	}
	select {
	case why := <-ended:
		t.Fatalf("after the fourth ping: %s", why)
	case <-time.After(time.Second):
	}
}

func readFramePayload(c net.Conn) (typ, flags byte, payload []byte, err error) {
	h := make([]byte, 9)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, 0, nil, err
	}
	payload = make([]byte, int(h[0])<<16|int(h[1])<<8|int(h[2]))
	if _, err := io.ReadFull(c, payload); err != nil {
		return 0, 0, nil, err
	}
	return h[3], h[4], payload, nil
}
