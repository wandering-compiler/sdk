package runtime

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// pingIdle opens a raw HTTP/2 connection to addr, sends `pings` pings
// `interval` apart with no stream open, and returns why the server ended the
// connection ("" when every ping was acknowledged and it stayed up).
func pingIdle(t *testing.T, addr string, interval time.Duration, pings int) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, c, 0x4, 0, nil)
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
	for i := range pings {
		if i > 0 {
			select {
			case why := <-ended:
				return why
			case <-time.After(interval):
			}
		}
		ping[7] = byte(i + 1)
		writeFrame(t, c, 0x6, 0, ping)
		select {
		case <-acks:
		case why := <-ended:
			return why
		case <-time.After(5 * time.Second):
			return "ping not acknowledged"
		}
	}
	select {
	case why := <-ended:
		return why
	case <-time.After(500 * time.Millisecond):
		return ""
	}
}

// The ServerOptions half of the keepalive pairing: a caller's enforcement
// policy in ServerOptions replaces the default (it is appended after it), in
// both directions. Pings every second are a violation of the default
// MinTime (10 s) and draw GOAWAY too_many_pings; a caller who permits them
// (MinTime 500 ms) keeps the connection. The override is not checked against
// the w17 clients' 30 s ping: one with a MinTime above it would GOAWAY every
// w17 client (a deliberate limit, S7).
func TestGRPCComponent_ACallersEnforcementPolicyReplacesTheDefault(t *testing.T) {
	noTLS(t)
	def := freeAddr(t)
	stop := runComponent(t, Config{Addr: def, RegisterServices: func(*grpc.Server) error { return nil }})
	defer func() { _ = stop() }()
	if why := pingIdle(t, def, time.Second, 4); why == "" {
		t.Error("the default policy permitted pings every second; it must enforce MinTime 10s")
	}

	over := freeAddr(t)
	stop2 := runComponent(t, Config{
		Addr:             over,
		RegisterServices: func(*grpc.Server) error { return nil },
		ServerOptions: []grpc.ServerOption{grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: 500 * time.Millisecond, PermitWithoutStream: true,
		})},
	})
	defer func() { _ = stop2() }()
	if why := pingIdle(t, over, time.Second, 4); why != "" {
		t.Errorf("the caller's policy (MinTime 500ms) did not replace the default: %s", why)
	}
}
