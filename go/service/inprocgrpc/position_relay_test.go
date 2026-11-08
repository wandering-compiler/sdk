package inprocgrpc_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// In a composed binary the business facade calls storage in-process. A call
// made with paging.Forward relays the page position the storage handler
// reports onto the facade's own response, as the dialled connection's
// interceptor does over the wire; a sibling call does not.
func TestConn_Invoke_RelaysThePagePositionOfAForwardedCall(t *testing.T) {
	storage := inprocgrpc.New()
	storage.RegisterService(unaryDesc("test.Query", "List", func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		paging.SetPosition(ctx, []*w17pb.KeysetValue{paging.FromInt64(3)}, []*w17pb.KeysetValue{paging.FromInt64(1)}, 9)
		return wrapperspb.String("page"), nil
	}), nil)
	storage.RegisterService(unaryDesc("test.Counts", "Count", func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		paging.SetPosition(ctx, []*w17pb.KeysetValue{paging.FromInt64(99)}, nil, 99)
		return wrapperspb.String("sibling"), nil
	}), nil)

	business := inprocgrpc.New()
	business.RegisterService(unaryDesc("test.Facade", "Browse", func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		out := new(wrapperspb.StringValue)
		if err := storage.Invoke(ctx, "/test.Counts/Count", wrapperspb.String(""), out); err != nil {
			return nil, err
		}
		if err := storage.Invoke(paging.Forward(ctx), "/test.Query/List", wrapperspb.String(""), out); err != nil {
			return nil, err
		}
		return out, nil
	}), nil)

	var header metadata.MD
	ctx := metadata.AppendToOutgoingContext(context.Background(), paging.LimitMDKey, "2")
	if err := business.Invoke(ctx, "/test.Facade/Browse", wrapperspb.String(""), new(wrapperspb.StringValue), grpc.Header(&header)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	pos, ok := paging.PositionFromHeader(header)
	if !ok || pos.Total != 9 || len(pos.Next) != 1 || paging.ScalarOf(pos.Next[0]) != int64(3) || len(pos.Prev) != 1 {
		t.Fatalf("the facade answered %v: want the forwarded List's position (next 3, total 9), not the sibling Count's", header)
	}
}
