package distx_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// mdOutcomeClient records what a generated inter-tier client would send for
// an Await: the call's outgoing metadata AFTER the principal relay every
// generated dial installs (core/grpcx.DialOpts → principal.ForwardToOutgoing).
type mdOutcomeClient struct{ sent metadata.MD }

func (c *mdOutcomeClient) Await(ctx context.Context, _ *distxpb.AwaitRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[distxpb.AwaitResponse], error) {
	c.sent, _ = metadata.FromOutgoingContext(principal.ForwardToOutgoing(ctx))
	return nil, status.Error(codes.NotFound, "unknown")
}

// The wait carries the routing token and nothing else: not the caller's
// principal, scopes or labels (a relay interceptor would otherwise copy them
// off the request), and not its transaction id — the Await is no call inside
// the transaction, and the py/ts/rs runtimes send exactly the token too.
func TestRemoteCommits_TheWaitCarriesOnlyTheRoutingToken(t *testing.T) {
	c := &mdOutcomeClient{}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		txregistry.HeaderName, "tx-1",
		distx.ConnIDHeader, "replica-7",
		"x-w17-user", "e30",
		principal.ScopeKey("org_id"), "org-1",
	))
	if got := distx.NewRemoteCommits(c).DeferRemote(ctx, "tx-1", func() {}); got != txregistry.EmitDropped {
		t.Fatalf("outcome %v, want EmitDropped for an unknown transaction", got)
	}
	want := metadata.Pairs(distx.ConnIDHeader, "replica-7")
	if len(c.sent) != len(want) || c.sent.Get(distx.ConnIDHeader)[0] != "replica-7" {
		t.Errorf("the Await carried %v, want only %v", c.sent, want)
	}
}
