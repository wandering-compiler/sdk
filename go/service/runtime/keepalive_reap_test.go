package runtime

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// The server half of the keepalive pairing: a client that stops answering
// is pinged after 30s of idleness and dropped 10s after the unanswered
// ping, so a dead peer does not hold a connection forever.
//
// The client is raw HTTP/2: it completes the handshake and acknowledges
// SETTINGS, but never acknowledges a PING — the shape of a peer that died
// without closing its socket. This takes ~40s by construction.
func TestGRPCComponent_PingsAnIdleClientAndDropsItWithoutAnAck(t *testing.T) {
	if testing.Short() {
		t.Skip("40s keepalive cycle")
	}
	noTLS(t)
	addr := freeAddr(t)
	stop := runComponent(t, Config{Addr: addr, RegisterServices: func(*grpc.Server) error { return nil }})
	defer func() { _ = stop() }()

	var c net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		if c, err = net.Dial("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, c, 0x4, 0, nil) // our SETTINGS, empty

	var pingAt time.Time
	_ = c.SetReadDeadline(start.Add(60 * time.Second))
	for {
		typ, flags, err := readFrame(c)
		if err != nil {
			if pingAt.IsZero() {
				t.Fatalf("connection ended (%v) after %v without a keepalive ping", err, time.Since(start).Round(time.Second))
			}
			if errors.Is(err, io.EOF) || isClosed(err) {
				if after := time.Since(pingAt); after < 8*time.Second || after > 15*time.Second {
					t.Errorf("dropped %v after the unanswered ping, want ~10s", after.Round(time.Second))
				}
				return
			}
			t.Fatalf("read after the ping: %v", err)
		}
		switch {
		case typ == 0x4 && flags&0x1 == 0:
			writeFrame(t, c, 0x4, 0x1, nil) // ack the server's SETTINGS
		case typ == 0x6 && flags&0x1 == 0 && pingAt.IsZero() && time.Since(start) > 20*time.Second:
			pingAt = time.Now()
			if idle := pingAt.Sub(start); idle > 35*time.Second {
				t.Errorf("first keepalive ping after %v of idleness, want ~30s", idle.Round(time.Second))
			}
		}
	}
}

func writeFrame(t *testing.T, c net.Conn, typ, flags byte, payload []byte) {
	t.Helper()
	h := make([]byte, 9, 9+len(payload)) // the 9-byte HTTP/2 frame header, then the payload
	h[0], h[1], h[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	h[3], h[4] = typ, flags
	if _, err := c.Write(append(h, payload...)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readFrame(c net.Conn) (typ, flags byte, err error) {
	h := make([]byte, 9)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, 0, err
	}
	n := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
	_ = binary.BigEndian.Uint32(h[5:]) // stream id, unused
	if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
		return 0, 0, err
	}
	return h[3], h[4], nil
}

func isClosed(err error) bool {
	var ne *net.OpError
	return errors.As(err, &ne) && !ne.Timeout()
}
