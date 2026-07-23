package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// A panicking tool handler is recovered into an MCP error result with
// no transport error, so the gateway process survives and the
// connection stays up (instead of the panic crashing the server).
func TestRecoverTool_PanicBecomesErrorResult(t *testing.T) {
	wrapped := recoverTool("explode", func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		panic("tool blew up")
	})

	res, err := wrapped(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil (panic must not surface as a transport error)", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("res = %+v, want a non-nil IsError result", res)
	}
}

// A normal handler passes through unchanged.
func TestRecoverTool_PassThrough(t *testing.T) {
	want := mcp.NewToolResultText("ok")
	wrapped := recoverTool("fine", func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return want, nil
	})
	res, err := wrapped(context.Background(), mcp.CallToolRequest{})
	if err != nil || res != want {
		t.Fatalf("pass-through = (%v, %v), want (ok-result, nil)", res, err)
	}
}
