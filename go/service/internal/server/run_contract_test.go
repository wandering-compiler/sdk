package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// A serve failure names the address it was serving.
func TestRun_ServeErrorNamesTheAddress(t *testing.T) {
	l, done := startRun(t, context.Background(), Config{
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return nil },
	})
	_ = l.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "serve on 127.0.0.1:0") {
			t.Fatalf("Run = %v, want the serve error wrapped with the address", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the listener failed")
	}
}

// OnShutdown runs exactly once, whether the server stopped because ctx was
// cancelled or because Serve failed on its own.
func TestRun_OnShutdownRunsExactlyOnce(t *testing.T) {
	for _, path := range []string{"ctx cancelled", "serve failed"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l, done := startRun(t, ctx, Config{
				Addr:             "127.0.0.1:0",
				RegisterServices: func(*grpc.Server) error { return nil },
				OnShutdown:       func() { calls.Add(1) },
			})
			if path == "ctx cancelled" {
				cancel()
			} else {
				_ = l.Close()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("OnShutdown ran %d times, want 1", n)
			}
		})
	}
}

// Serving reflection without authentication is said at start, naming the
// address.
func TestRun_UnauthenticatedReflectionIsLogged(t *testing.T) {
	buf := &lockedBuffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(orig)
	addr := runOnLoopback(t, Config{Reflection: true, ReflectionUnauthenticated: true})
	// Reflection answering means Run got past the point that logs.
	if v1, _ := listServices(t, addr, nil); v1 != nil {
		t.Fatalf("reflection: %v", v1)
	}
	if got := buf.String(); !strings.Contains(got, "served WITHOUT authentication") || !strings.Contains(got, "127.0.0.1:") {
		t.Errorf("no warning naming the address for unauthenticated reflection; log: %q", got)
	}
}

// Nothing is served while services are still being registered: a client
// that connects then gets no HTTP/2 response, and it gets one once
// registration has returned.
func TestRun_DoesNotServeBeforeRegistrationReturns(t *testing.T) {
	registering, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, done := startRun(t, ctx, Config{
		Addr: "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error {
			close(registering)
			<-release
			return nil
		},
	})
	<-registering
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 9)
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if n, _ := c.Read(buf); n > 0 {
		t.Fatal("the server answered HTTP/2 before RegisterServices returned")
	}
	close(release)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("no HTTP/2 response after registration returned: %v", err)
	}
	_ = c.Close()
	cancel()
	<-done
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
