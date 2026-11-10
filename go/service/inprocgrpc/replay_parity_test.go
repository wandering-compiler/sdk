package inprocgrpc_test

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// A handler's own UNAVAILABLE runs the handler once in-process, as it does
// over the wire, where the w17 server marks it answered and the w17 clients
// do not replay it (sdk/go/service/runtime
// TestGRPCComponent_AHandlersUnavailableIsNotReplayed): the composed and the
// split topology agree.
func TestConn_AHandlersUnavailableRunsOnce(t *testing.T) {
	var runs atomic.Int32
	conn := inprocgrpc.New()
	conn.RegisterService(unaryDesc("test.Record", "Record", func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		runs.Add(1)
		return nil, status.Error(codes.Unavailable, "the upstream went away after the write")
	}), nil)
	err := conn.Invoke(context.Background(), recordMethod, wrapperspb.String("x"), new(wrapperspb.StringValue))
	if status.Code(err) != codes.Unavailable || runs.Load() != 1 {
		t.Fatalf("caller saw %v after %d runs, want the handler's UNAVAILABLE after one", err, runs.Load())
	}
}
