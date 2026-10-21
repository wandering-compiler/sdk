package mcp_test

import (
	"net/http"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/mcp"
)

// The gateway's own context (an audience gateway's x-w17-audience) is written
// after every client source: a client sending the same key does not choose
// the audience it is admitted through.
func TestCollectAuthHeaders_GatewayHeadersWin(t *testing.T) {
	conn := mcp.ConnectionInfo{
		HTTPHeaders:      http.Header{"X-W17-Audience": []string{"staff"}},
		InitializeParams: map[string]any{"x-w17-membership-kinds": "staff"},
		Gateway:          map[string]string{"x-w17-audience": "client", "x-w17-membership-kinds": "client"},
	}
	got := mcp.CollectAuthHeaders(conn)
	if got["x-w17-audience"] != "client" || got["x-w17-membership-kinds"] != "client" {
		t.Errorf("gateway headers lost to the client's: %v", got)
	}
}
