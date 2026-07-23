package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mcpStub is a configurable MCP streamable-HTTP server stand-in. It mints a
// session on `initialize` and serves a canned body for `tools/call`, so the
// MCPCaller can be driven end-to-end without a real gateway.
type mcpStub struct {
	sessionHeader string // value to emit on initialize ("" → omit, exercises the no-header error)
	initStatus    int    // status for initialize (0 → 200)
	callStatus    int    // status for tools/call (0 → 200)
	callBody      string // body for tools/call
	initCount     int
	callCount     int
}

func (s *mcpStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &req)
		switch req.Method {
		case "initialize":
			s.initCount++
			if s.sessionHeader != "" {
				w.Header().Set(mcpSessionHeader, s.sessionHeader)
			}
			if s.initStatus != 0 {
				w.WriteHeader(s.initStatus)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			s.callCount++
			if s.callStatus != 0 {
				w.WriteHeader(s.callStatus)
			}
			_, _ = w.Write([]byte(s.callBody))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func newMCPStub(t *testing.T, s *mcpStub) (*MCPCaller, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return NewMCPCaller(srv.URL, nil), srv
}

func TestNewMCPCaller_DefaultClient(t *testing.T) {
	if c := NewMCPCaller("http://x/mcp", nil); c.Client != http.DefaultClient {
		t.Error("nil client should default to http.DefaultClient")
	}
	custom := &http.Client{}
	if c := NewMCPCaller("http://x/mcp", custom); c.Client != custom {
		t.Error("explicit client should be retained")
	}
}

func mcpEndpoint() Endpoint {
	return Endpoint{Ref: "x.Svc.Tool", Transport: "mcp", ToolName: "do_thing"}
}

func TestMCPCall_StructuredContent(t *testing.T) {
	s := &mcpStub{sessionHeader: "sess-1", callBody: `{"result":{"structuredContent":{"id":"7","ok":true}}}`}
	c, _ := newMCPStub(t, s)
	out, err := c.Call(context.Background(), mcpEndpoint(), map[string]any{"a": 1}, "", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out["id"] != "7" || out["ok"] != true {
		t.Errorf("structuredContent unwrap = %v", out)
	}
	// session reused: a second call must not re-initialize
	if _, err := c.Call(context.Background(), mcpEndpoint(), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if s.initCount != 1 {
		t.Errorf("initialize ran %d times, want 1 (session cached)", s.initCount)
	}
	if s.callCount != 2 {
		t.Errorf("tools/call ran %d times, want 2", s.callCount)
	}
}

func TestMCPCall_TextContent(t *testing.T) {
	s := &mcpStub{sessionHeader: "sess-1", callBody: `{"result":{"content":[{"type":"text","text":"{\"v\":42}"}]}}`}
	c, _ := newMCPStub(t, s)
	out, err := c.Call(context.Background(), mcpEndpoint(), nil, "tok", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !looseFloatEq(out["v"], 42) {
		t.Errorf("text content unwrap = %v", out)
	}
}

func TestMCPCall_SSEFraming(t *testing.T) {
	// streamable-HTTP SSE frame instead of a bare JSON body
	s := &mcpStub{sessionHeader: "sess-1", callBody: "event: message\ndata: {\"result\":{\"structuredContent\":{\"ok\":true}}}\n\n"}
	c, _ := newMCPStub(t, s)
	out, err := c.Call(context.Background(), mcpEndpoint(), nil, "", nil)
	if err != nil {
		t.Fatalf("Call SSE: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("SSE unwrap = %v", out)
	}
}

func TestMCPCall_ErrorsFromServer(t *testing.T) {
	cases := []struct {
		name string
		stub *mcpStub
	}{
		{"init status >=400", &mcpStub{sessionHeader: "s", initStatus: 500}},
		{"no session header", &mcpStub{sessionHeader: ""}},
		{"call status >=400", &mcpStub{sessionHeader: "s", callStatus: 500, callBody: "boom"}},
		{"decode envelope error", &mcpStub{sessionHeader: "s", callBody: "}not-json{"}},
		{"tool error field", &mcpStub{sessionHeader: "s", callBody: `{"error":{"code":7,"message":"bad"}}`}},
		{"empty result", &mcpStub{sessionHeader: "s", callBody: `{"jsonrpc":"2.0"}`}},
		{"isError flag", &mcpStub{sessionHeader: "s", callBody: `{"result":{"isError":true}}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caller, _ := newMCPStub(t, c.stub)
			if _, err := caller.Call(context.Background(), mcpEndpoint(), nil, "", nil); err == nil {
				t.Errorf("%s: want error", c.name)
			}
		})
	}
}

func TestMCPCall_TransportError(t *testing.T) {
	// point at a closed server → initialize transport error propagates
	srv := httptest.NewServer((&mcpStub{sessionHeader: "s", callBody: "{}"}).handler())
	url := srv.URL
	srv.Close()
	c := NewMCPCaller(url, nil)
	if _, err := c.Call(context.Background(), mcpEndpoint(), nil, "", nil); err == nil {
		t.Error("transport error should propagate")
	}
}

func TestMCPCall_MarshalArgumentsError(t *testing.T) {
	// session establishes fine, then the tools/call rpc fails to marshal
	// because an argument value (a channel) is not JSON-serialisable.
	c, _ := newMCPStub(t, &mcpStub{sessionHeader: "s", callBody: "{}"})
	bad := map[string]any{"ch": make(chan int)}
	if _, err := c.Call(context.Background(), mcpEndpoint(), bad, "", nil); err == nil {
		t.Error("unmarshalable arguments should fail")
	}
}

func TestMCPCall_ToolsCallTransportError(t *testing.T) {
	// initialize succeeds; the tools/call request then hits a hijacked,
	// abruptly-closed connection → Client.Do errors.
	mu := &sync.Mutex{}
	var initDone bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set(mcpSessionHeader, "s")
			_, _ = w.Write([]byte(`{"result":{}}`))
			mu.Lock()
			initDone = true
			mu.Unlock()
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default: // tools/call → hijack + close to break the response
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	c := NewMCPCaller(srv.URL, nil)
	if _, err := c.Call(context.Background(), mcpEndpoint(), nil, "", nil); err == nil {
		t.Error("tools/call transport error should propagate")
	}
	mu.Lock()
	defer mu.Unlock()
	if !initDone {
		t.Error("initialize should have completed before the failing tools/call")
	}
}

func TestMCPCall_BuildRequestError(t *testing.T) {
	// an un-parseable endpoint fails http.NewRequestWithContext inside
	// ensureSession (surfaced through Call).
	c := NewMCPCaller("http://%zz", nil)
	if _, err := c.Call(context.Background(), mcpEndpoint(), nil, "", nil); err == nil {
		t.Error("bad endpoint URL should fail request build")
	}
}

func TestSSEUnwrap(t *testing.T) {
	// plain JSON passes through unchanged
	plain := []byte(`{"a":1}`)
	if got := sseUnwrap(plain); string(got) != string(plain) {
		t.Errorf("plain passthrough = %q", got)
	}
	// SSE frame → concatenated data payload
	frame := []byte("event: message\ndata: {\"a\":\ndata: 1}\n\n")
	if got := string(sseUnwrap(frame)); got != `{"a":1}` {
		t.Errorf("SSE concat = %q", got)
	}
	// data: lines that are all empty → returns raw unchanged
	emptyData := []byte("\ndata:\n")
	if got := sseUnwrap(emptyData); string(got) != string(emptyData) {
		t.Errorf("empty data should return raw = %q", got)
	}
}

func TestUnwrapToolResult(t *testing.T) {
	// neither structuredContent nor a text block → empty object
	out, err := unwrapToolResult("ref", &toolResult{})
	if err != nil || len(out) != 0 {
		t.Errorf("empty result = %v,%v; want {},nil", out, err)
	}
	// a non-text content block is skipped, falling through to empty
	r := &toolResult{}
	r.Content = append(r.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "image", Text: "ignored"})
	out, err = unwrapToolResult("ref", r)
	if err != nil || len(out) != 0 {
		t.Errorf("non-text content = %v,%v; want {},nil", out, err)
	}
}

// looseFloatEq compares a JSON-decoded number (float64) to an int literal.
func looseFloatEq(v any, want int) bool {
	f, ok := v.(float64)
	return ok && f == float64(want)
}
