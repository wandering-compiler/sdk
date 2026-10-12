package distx_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// The edges refuse to fill the connection-affinity key from client input,
// matching it by the same literal; a rename must move both.
func TestConnIDHeader_IsReservedAtTheEdge(t *testing.T) {
	if !principal.IsEdgeReservedKey(distx.ConnIDHeader) {
		t.Fatalf("principal.IsEdgeReservedKey(%q) = false: a client could choose the replica a transaction is routed to — update transportKeys in lib/principal/reserved.go",
			distx.ConnIDHeader)
	}
}
