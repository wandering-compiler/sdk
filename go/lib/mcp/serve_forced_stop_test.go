package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// A tool call still running when the stop bound runs out is cut: its
// context is cancelled and ServeStreamableHTTP returns by the deadline,
// without an error — the bounded Shutdown the transport promises, with
// mcp-go's own teardown run first.
func TestServeStreamableHTTP_ForceClosesAToolCallPastTheBound(t *testing.T) {
	t.Setenv("W17_MCP_SHUTDOWN_TIMEOUT_SECONDS", "1")
	addr := freeAddr(t)
	s := NewServer("t", "1", nil)
	started, cut := make(chan struct{}), make(chan struct{})
	s.AddTool("hang", "runs until cancelled", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cut)
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.ServeStreamableHTTP(ctx, addr) }()

	post := func(body string, session string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+EndpointPath, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		return http.DefaultClient.Do(req)
	}
	var session string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		resp, err := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, "")
		if err == nil {
			session = resp.Header.Get("Mcp-Session-Id")
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport never came up: %v", err)
		}
	}
	go func() {
		if resp, err := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hang","arguments":{}}}`, session); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool call never started")
	}

	stopAt := time.Now()
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("ServeStreamableHTTP = %v; a forced close is not a failed shutdown", err)
		}
		if took := time.Since(stopAt); took > 4*time.Second {
			t.Errorf("returned %v after the cancel, want ~the 1s bound", took.Round(100*time.Millisecond))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStreamableHTTP did not return: the hanging tool call held it")
	}
	select {
	case <-cut:
	case <-time.After(5 * time.Second):
		t.Error("the hanging tool call's context was never cancelled")
	}
}
