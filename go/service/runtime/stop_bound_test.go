package runtime

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/emptypb"
)

// holdUntilStopDesc is a server-streaming method whose handler runs until its
// context ends: a graceful stop cannot finish while it is in flight.
func holdUntilStopDesc(started chan<- struct{}) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Hold",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Hold",
			ServerStreams: true,
			Handler: func(_ any, ss grpc.ServerStream) error {
				close(started)
				<-ss.Context().Done()
				return ss.Context().Err()
			},
		}},
	}
}

// The component stops within ShutdownTimeout of the cancel even when a
// handler would keep the graceful drain waiting.
func TestGRPCComponent_StopsWithinShutdownTimeout(t *testing.T) {
	noTLS(t)
	addr := freeAddr(t)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- GRPCComponent(Config{
			Addr:             addr,
			ShutdownTimeout:  time.Second,
			RegisterServices: func(s *grpc.Server) error { s.RegisterService(holdUntilStopDesc(started), struct{}{}); return nil },
		}).Run(ctx)
	}()

	conn := plainConn(t, addr)
	var cs grpc.ClientStream
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		cs, err = conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Hold", ServerStreams: true}, "/test.Hold/Hold", grpc.WaitForReady(true))
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	stopAt := time.Now()
	cancel()
	select {
	case <-done:
		if took := time.Since(stopAt); took < 800*time.Millisecond || took > 3*time.Second {
			t.Errorf("stopped %v after the cancel, want ~ShutdownTimeout (1s)", took.Round(100*time.Millisecond))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the component did not stop")
	}
}

// A caller's ServerOptions come after the built-in ones, so its keepalive
// wins: with a 1s ping interval and a 1s ack timeout, a client that never
// acknowledges a PING is pinged within seconds and dropped soon after.
func TestGRPCComponent_CallerKeepaliveOverridesTheDefault(t *testing.T) {
	noTLS(t)
	addr := freeAddr(t)
	stop := runComponent(t, Config{
		Addr:             addr,
		RegisterServices: func(*grpc.Server) error { return nil },
		ServerOptions:    []grpc.ServerOption{grpc.KeepaliveParams(keepalive.ServerParameters{Time: time.Second, Timeout: time.Second})},
	})
	defer func() { _ = stop() }()

	var c net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err = net.Dial("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, c, 0x4, 0, nil)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	for {
		typ, flags, err := readFrame(c)
		if err != nil {
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("dropped after %v, want within a few seconds under the caller's 1s keepalive", took.Round(time.Second))
			}
			return
		}
		if typ == 0x4 && flags&0x1 == 0 {
			writeFrame(t, c, 0x4, 0x1, nil)
		}
	}
}
