package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Tool arguments keep their numbers exactly as the client wrote them.
//
// mcp-go decodes `tools/call` into a struct whose Arguments field is `any`,
// so every JSON number in the arguments becomes a float64 before a handler
// sees it. A 64-bit id sent as a number (protojson accepts them unquoted, and
// the REST surface keeps them exact) loses everything past 2^53:
// 9007199254740993 reaches the backend as 9007199254740992 — a different
// record, with no error.
//
// The raw message is still in hand one step earlier, in the request
// initialization hook, so the arguments are captured there verbatim and put
// back on the request in the before-call hook, where CallUnary picks them up
// through GetRawArguments. The two hooks are correlated by the request id
// within one argScope: one per HTTP request (the streamable transport handles
// exactly one message per POST) and one per stdio connection (one client).
// Without a scope on the context — a caller driving HandleMessage itself —
// nothing is captured and the library's decoded arguments are used.

type argScopeCtxKey struct{}

// argScope holds the raw arguments of the tool calls in flight on one
// connection or request, keyed by JSON-RPC id.
type argScope struct {
	mu   sync.Mutex
	byID map[string]json.RawMessage
}

func withArgScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, argScopeCtxKey{}, &argScope{})
}

func argScopeFrom(ctx context.Context) *argScope {
	sc, _ := ctx.Value(argScopeCtxKey{}).(*argScope)
	return sc
}

func (sc *argScope) put(key string, raw json.RawMessage) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.byID == nil {
		sc.byID = map[string]json.RawMessage{}
	}
	sc.byID[key] = raw
}

func (sc *argScope) take(key string) (json.RawMessage, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	raw, ok := sc.byID[key]
	if ok {
		delete(sc.byID, key)
	}
	return raw, ok
}

// requestIDKey renders a JSON-RPC id as a map key. The library hands both
// hooks the same decoded value (a string or a float64); anything else is not
// a valid id and is not correlated.
func requestIDKey(id any) (string, bool) {
	switch v := id.(type) {
	case string:
		return "s:" + v, true
	case float64:
		return "n:" + strconv.FormatFloat(v, 'g', -1, 64), true
	case json.Number:
		return "n:" + v.String(), true
	case int64:
		return "n:" + strconv.FormatInt(v, 10), true
	}
	return "", false
}

// exactArgumentHooks wires the capture and the hand-back.
func exactArgumentHooks() *mcpserver.Hooks {
	h := &mcpserver.Hooks{}
	h.AddOnRequestInitialization(func(ctx context.Context, id any, message any) error {
		sc := argScopeFrom(ctx)
		if sc == nil {
			return nil
		}
		raw, ok := message.(json.RawMessage)
		if !ok {
			return nil
		}
		key, ok := requestIDKey(id)
		if !ok {
			return nil
		}
		var call struct {
			Method string `json:"method"`
			Params struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		// A message this cannot read is left to the library, which answers it.
		if json.Unmarshal(raw, &call) != nil || call.Method != string(mcp.MethodToolsCall) {
			return nil
		}
		if len(call.Params.Arguments) == 0 || string(call.Params.Arguments) == "null" {
			return nil
		}
		sc.put(key, call.Params.Arguments)
		return nil
	})
	h.AddBeforeCallTool(func(ctx context.Context, id any, req *mcp.CallToolRequest) {
		sc := argScopeFrom(ctx)
		if sc == nil || req == nil {
			return
		}
		key, ok := requestIDKey(id)
		if !ok {
			return
		}
		if raw, ok := sc.take(key); ok {
			req.Params.Arguments = raw
		}
	})
	// A tools/call the library refused before the before-call hook (an
	// unparsable request) must not leave its capture behind.
	h.AddOnError(func(ctx context.Context, id any, method mcp.MCPMethod, _ any, _ error) {
		if method != mcp.MethodToolsCall {
			return
		}
		if sc := argScopeFrom(ctx); sc != nil {
			if key, ok := requestIDKey(id); ok {
				sc.take(key)
			}
		}
	})
	return h
}
