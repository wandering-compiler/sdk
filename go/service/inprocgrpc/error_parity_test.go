package inprocgrpc_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

const failMethod = "/test.Fail/Fail"

// failDesc answers each request value with a different kind of error.
func failDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Fail",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Fail",
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				switch in.GetValue() {
				case "detailed":
					st, _ := status.New(codes.FailedPrecondition, "order is closed").WithDetails(&errdetails.ErrorInfo{Reason: "ORDER_CLOSED", Domain: "shop"})
					return nil, st.Err()
				case "plain":
					return nil, errors.New("a bare error a handler should not return")
				case "deadline":
					return nil, context.DeadlineExceeded
				}
				return nil, status.Error(codes.NotFound, "no such order")
			},
		}},
	}
}

// A failure reaches the caller as the same status — code, message and
// details — whether the tier is called in-process or over the wire, so the
// gateway maps it to the same HTTP response in a composed binary and in a
// split deployment.
func TestConn_ErrorsMatchTheWire(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(failDesc(), struct{}{})

	srv := grpc.NewServer()
	srv.RegisterService(failDesc(), struct{}{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	wire, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wire.Close() }()

	for _, kind := range []string{"detailed", "plain", "deadline", "notfound"} {
		t.Run(kind, func(t *testing.T) {
			inErr := conn.Invoke(context.Background(), failMethod, wrapperspb.String(kind), new(wrapperspb.StringValue))
			wireErr := wire.Invoke(context.Background(), failMethod, wrapperspb.String(kind), new(wrapperspb.StringValue))
			in, wi := status.Convert(inErr), status.Convert(wireErr)
			if in.Code() != wi.Code() || in.Message() != wi.Message() {
				t.Errorf("in-process %v %q, wire %v %q", in.Code(), in.Message(), wi.Code(), wi.Message())
			}
			if !proto.Equal(in.Proto(), wi.Proto()) {
				t.Errorf("details differ:\n in-process %v\n wire       %v", in.Proto(), wi.Proto())
			}
		})
	}
}
