package mcp

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/service/healthcheck"
)

// The MCP listener answers the `<binary> health` probe. It had no liveness
// endpoint at all, so a container HEALTHCHECK over it could only ever fail.
// Driven through the probe itself, not a hand-rolled GET, so the path and the
// expected status are the probe's and cannot drift from what is tested here.
func TestServeStreamableHTTPAnswersHealthProbe(t *testing.T) {
	addr := freeAddr(t)
	s := NewServer("t", "1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.ServeStreamableHTTP(ctx, addr) }()

	var err error
	for i := 0; i < 100; i++ {
		err = healthcheck.Run(context.Background(), nil, healthcheck.Options{
			Probes: []healthcheck.Probe{healthcheck.HTTP("mcp", addr)},
			Out:    io.Discard,
		})
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("MCP listener never answered the health probe: %v", err)
}
