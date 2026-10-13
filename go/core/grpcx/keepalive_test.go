package grpcx

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// Both w17 dialers ping on the same schedule, the one the server's
// enforcement is proven to permit
// (TestGRPCComponent_PermitsTheClientsKeepalivePingsOnAnIdleConnection).
func TestDefaultClientKeepalive_MatchesTheLibraryClient(t *testing.T) {
	if defaultClientKeepalive != grpcclient.DefaultClientKeepalive {
		t.Fatalf("grpcx keepalive %+v differs from grpcclient's %+v", defaultClientKeepalive, grpcclient.DefaultClientKeepalive)
	}
}
