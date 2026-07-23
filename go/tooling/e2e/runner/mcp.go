package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// mcpSessionHeader is the streamable-HTTP MCP session key. The server
// (mark3labs/mcp-go) mints it on `initialize` and rejects every later
// request that omits it with `404 Invalid session ID`.
const mcpSessionHeader = "Mcp-Session-Id"

// mcpProtocolVersion is the MCP version the e2e client negotiates in
// `initialize`. Pinned to a value the server supports; the handshake
// echoes it back.
const mcpProtocolVersion = "2024-11-05"

// MCPCaller drives the MCP transport as a JSON-RPC `tools/call` over
// HTTP (the streamable-HTTP shape). The `input` is sent verbatim as
// the tool arguments; the tool result is unwrapped into the same
// string-keyed object the matchers assert against.
//
// PROVISIONAL (iter-1): the concrete MCP transport + result envelope
// are still an open decision in the spec. This implementation targets
// the common shape — JSON-RPC over a single HTTP endpoint, result with
// `structuredContent` (preferred) or a JSON text content block — and
// is the swap point when the envelope is finalised.
type MCPCaller struct {
	Endpoint string // the MCP HTTP endpoint, e.g. http://localhost:8080/mcp
	Client   *http.Client

	mu        sync.Mutex
	sessionID string // negotiated once via initialize, reused for every tools/call
}

// NewMCPCaller builds an MCP caller against the MCP HTTP endpoint.
func NewMCPCaller(endpoint string, client *http.Client) *MCPCaller {
	if client == nil {
		client = http.DefaultClient
	}
	return &MCPCaller{Endpoint: endpoint, Client: client}
}

type jsonrpcReq struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type jsonrpcResp struct {
	Result *toolResult `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ensureSession runs the MCP `initialize` handshake once and caches the
// server-minted session id. Streamable-HTTP MCP rejects any
// `tools/call` that lacks the `Mcp-Session-Id` header, so this must
// precede the first tool call. Idempotent + mutex-guarded — the runner
// shares one MCPCaller across every MCP step.
func (c *MCPCaller) ensureSession(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != "" {
		return c.sessionID, nil
	}
	rpc := jsonrpcReq{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "w17-e2e", "version": "1"},
		},
	}
	b, _ := json.Marshal(rpc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("mcp initialize: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcp initialize: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("mcp initialize: status %d: %s", resp.StatusCode, truncate(raw, 256))
	}
	sid := resp.Header.Get(mcpSessionHeader)
	if sid == "" {
		return "", fmt.Errorf("mcp initialize: server returned no %s header", mcpSessionHeader)
	}
	c.sessionID = sid
	// Best-effort `notifications/initialized` — spec-mandated, but the
	// server accepts tool calls without it; ignore transport errors.
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if nreq, nerr := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(note)); nerr == nil {
		nreq.Header.Set("Content-Type", "application/json")
		nreq.Header.Set("Accept", "application/json, text/event-stream")
		nreq.Header.Set(mcpSessionHeader, sid)
		if nresp, derr := c.Client.Do(nreq); derr == nil {
			nresp.Body.Close()
		}
	}
	return sid, nil
}

// headers is accepted to satisfy the Caller interface; the MCP transport
// has no per-call header surface today (the streamable-HTTP session owns
// its headers), so static step headers are a no-op here.
func (c *MCPCaller) Call(ctx context.Context, ep Endpoint, input map[string]any, token string, _ map[string]string) (map[string]any, error) {
	sid, err := c.ensureSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", ep.Ref, err)
	}
	rpc := jsonrpcReq{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  map[string]any{"name": ep.ToolName, "arguments": input},
	}
	b, err := json.Marshal(rpc)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: marshal: %w", ep.Ref, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: build request: %w", ep.Ref, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpSessionHeader, sid)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", ep.Ref, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp %s: status %d: %s", ep.Ref, resp.StatusCode, truncate(raw, 512))
	}

	var out jsonrpcResp
	if err := json.Unmarshal(sseUnwrap(raw), &out); err != nil {
		return nil, fmt.Errorf("mcp %s: decode envelope: %w (body: %s)", ep.Ref, err, truncate(raw, 256))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s: tool error %d: %s", ep.Ref, out.Error.Code, out.Error.Message)
	}
	if out.Result == nil {
		return nil, fmt.Errorf("mcp %s: empty result", ep.Ref)
	}
	if out.Result.IsError {
		return nil, fmt.Errorf("mcp %s: tool reported isError", ep.Ref)
	}
	return unwrapToolResult(ep.Ref, out.Result)
}

// sseUnwrap returns the JSON-RPC payload from a response body that may
// be either a bare JSON object or a streamable-HTTP SSE frame
// (`event: message\ndata: {...}\n\n`). When the body carries `data:`
// lines it concatenates their content (the JSON-RPC message); a plain
// JSON body is returned unchanged. Robustness seam — mark3labs returns
// JSON for tool calls today, but the streamable-HTTP transport is free
// to switch to SSE framing per the Accept header.
func sseUnwrap(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(s, "data:") && !strings.Contains(s, "\ndata:") {
		return raw
	}
	var data strings.Builder
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	if data.Len() == 0 {
		return raw
	}
	return []byte(data.String())
}

// unwrapToolResult extracts the asserted object from a tool result:
// structuredContent when present, else the first text content block
// parsed as JSON.
func unwrapToolResult(ref string, r *toolResult) (map[string]any, error) {
	if len(r.StructuredContent) > 0 {
		return decodeObject(r.StructuredContent)
	}
	for _, c := range r.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return decodeObject([]byte(c.Text))
		}
	}
	return map[string]any{}, nil
}
