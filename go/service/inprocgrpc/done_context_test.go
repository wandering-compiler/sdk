package inprocgrpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// doneContexts are the two ways a caller's context is already over.
func doneContexts() map[string]struct {
	ctx  context.Context
	want codes.Code
} {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	_ = cancel2
	return map[string]struct {
		ctx  context.Context
		want codes.Code
	}{
		"cancelled": {cancelled, codes.Canceled},
		"expired":   {expired, codes.DeadlineExceeded},
	}
}

// wireEchoClient serves the same echo service over a real gRPC transport, so
// the in-process answer can be compared with the wire's rather than with what
// this test believes the wire does.
func wireEchoClient(t *testing.T) grpc.ClientConnInterface {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	gs.RegisterService(echoServiceDesc(), &echoServer{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// A call on a done context never reaches the handler, and answers what the wire
// answers. It used to run in-process with the dead context — and a cancelled
// caller's transaction committed in a composed binary while it rolled back over
// the wire.
func TestInvoke_RefusesADoneContextLikeTheWire(t *testing.T) {
	ran := false
	conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			ran = true
			return handler(ctx, req)
		}))
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	wire := wireEchoClient(t)

	for name, c := range doneContexts() {
		t.Run(name, func(t *testing.T) {
			ran = false
			got := conn.Invoke(c.ctx, echoMethod, wrapperspb.String("x"), new(wrapperspb.StringValue))
			if ran {
				t.Error("the handler ran on a context that was already done")
			}
			if status.Code(got) != c.want {
				t.Errorf("in-process code = %v, want %v (err=%v)", status.Code(got), c.want, got)
			}
			wireErr := wire.Invoke(c.ctx, echoMethod, wrapperspb.String("x"), new(wrapperspb.StringValue))
			if status.Code(wireErr) != status.Code(got) {
				t.Errorf("transports disagree: wire %v, in-process %v", status.Code(wireErr), status.Code(got))
			}
		})
	}
}

// Over the wire a done call never reaches a server that could say
// Unimplemented, so the context's answer wins here too.
func TestInvoke_DoneContextWinsOverAnUnknownMethod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := inprocgrpc.New().Invoke(ctx, "/no.Such/Method", wrapperspb.String("x"), new(wrapperspb.StringValue))
	if status.Code(err) != codes.Canceled {
		t.Errorf("code = %v, want Canceled", status.Code(err))
	}
}

// The streaming half: no stream is opened and no handler goroutine starts.
func TestNewStream_RefusesADoneContext(t *testing.T) {
	ran := false
	conn := inprocgrpc.New(inprocgrpc.WithStreamInterceptor(
		func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			ran = true
			return handler(srv, ss)
		}))
	conn.RegisterService(countServiceDesc(), &countServer{})

	for name, c := range doneContexts() {
		t.Run(name, func(t *testing.T) {
			cs, err := conn.NewStream(c.ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
			if status.Code(err) != c.want || cs != nil {
				t.Fatalf("NewStream = (%v, %v), want (nil, %v)", cs, err, c.want)
			}
			// The handler goroutine would have started synchronously enough to
			// be observed by now if NewStream had dispatched.
			time.Sleep(20 * time.Millisecond)
			if ran {
				t.Error("a stream handler ran on a context that was already done")
			}
		})
	}
}

// Control: a live context still dispatches, or the tests above would pass for a
// Conn that refused everything.
func TestInvoke_LiveContextStillDispatches(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.GetValue() != "echo:x" {
		t.Errorf("reply = %q, want echo:x", out.GetValue())
	}
}
