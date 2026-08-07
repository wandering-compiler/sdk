package grpcrollback_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
)

// TestInterceptor_MetadataWithoutTxID covers readTxID's "metadata present but
// the w17-tx-id key is absent" arm (distinct from the no-metadata-at-all and
// the present-but-empty-value cases): md.Get returns a zero-length slice, so
// no rollback is attempted on a handler error.
func TestInterceptor_MetadataWithoutTxID(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-other-header", "v"))
	handler := func(context.Context, any) (any, error) { return nil, context.Canceled }

	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler); err == nil {
		t.Fatal("want the handler error propagated")
	}
	if len(roller.rolledBack) != 0 {
		t.Errorf("no tx-id header → must not roll back, got %v", roller.rolledBack)
	}
}
