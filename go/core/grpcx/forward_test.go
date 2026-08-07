package grpcx

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestForwardPrincipalUnaryInterceptor confirms the interceptor DialOpts
// installs relays the principal from incoming to outgoing metadata (so the
// storage tier sees the scope over the wire) while leaving non-principal
// metadata alone.
func TestForwardPrincipalUnaryInterceptor(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-scope-org_id", "org-7", "x-w17-paging-limit", "10"))

	var seen context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		seen = ctx
		return nil
	}
	if err := forwardPrincipalUnaryInterceptor(ctx, "/svc/M", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	out, ok := metadata.FromOutgoingContext(seen)
	if !ok {
		t.Fatal("interceptor set no outgoing metadata")
	}
	if got := out.Get("x-w17-scope-org_id"); len(got) != 1 || got[0] != "org-7" {
		t.Errorf("scope not relayed to outgoing: %v", got)
	}
	if got := out.Get("x-w17-paging-limit"); len(got) != 0 {
		t.Errorf("paging metadata must not be relayed: %v", got)
	}
}
