package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// Each tool call is one request for the error tracker: it runs on its own
// Sentry hub, so breadcrumbs one call records never reach another call's
// report.
func TestAddTool_EachCallRunsInItsOwnReportingScope(t *testing.T) {
	if err := observx.MustInit(observx.Config{ServiceName: "svc", SentryDSN: "https://abc123@127.0.0.1/1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observx.Shutdown(ctx)
	})

	s := NewServer("svc", "1", nil)
	var hubs []*sentry.Hub
	s.AddTool("probe", "records its hub", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hubs = append(hubs, sentry.GetHubFromContext(ctx))
		return mcp.NewToolResultText("ok"), nil
	})
	for i := 0; i < 2; i++ {
		msg := json.RawMessage(`{"jsonrpc":"2.0","id":` + string(rune('1'+i)) + `,"method":"tools/call","params":{"name":"probe","arguments":{}}}`)
		if resp := s.inner.HandleMessage(context.Background(), msg); resp == nil {
			t.Fatal("tools/call returned nothing")
		}
	}
	if len(hubs) != 2 || hubs[0] == nil || hubs[1] == nil {
		t.Fatalf("a tool call ran without its own hub: %v", hubs)
	}
	if hubs[0] == hubs[1] || hubs[0] == sentry.CurrentHub() {
		t.Error("tool calls share a hub")
	}
}
