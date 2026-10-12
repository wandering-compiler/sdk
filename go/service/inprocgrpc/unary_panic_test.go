package inprocgrpc_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// A unary handler that panics answers a generic Internal — the panic value
// never reaches the caller — and the bridge keeps serving, as a wire server
// behind its recovery interceptor does.
func TestConn_UnaryPanicBecomesInternalAndTheProcessSurvives(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})

	err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("panic"), new(wrapperspb.StringValue))
	if status.Code(err) != codes.Internal {
		t.Fatalf("Invoke of a panicking handler = %v, want Internal", err)
	}
	if strings.Contains(err.Error(), "secret-internal-state") {
		t.Errorf("the panic value reached the caller: %v", err)
	}

	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("hi"), out); err != nil || out.GetValue() != "echo:hi" {
		t.Fatalf("the next call after a panic = %q, %v; want echo:hi", out.GetValue(), err)
	}
}
