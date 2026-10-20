package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// restFake answers the consent RPCs as the auth plugin would, for the
// session "sess".
type restFake struct {
	approvedFor []string
	codes       map[string]string // code → session
}

func (r *restFake) Call(_ context.Context, ep Endpoint, input map[string]any, token string, _ map[string]string, _ string, _ []FilePart) (map[string]any, error) {
	if token != "sess" {
		return nil, &CallError{Code: "UNAUTHENTICATED"}
	}
	switch ep.Ref {
	case "auth.AuthService.GetOAuthAuthorization":
		return map[string]any{"orgs": []any{map[string]any{"id": "org-1"}}}, nil
	case "auth.AuthService.ApproveOAuthAuthorization":
		r.approvedFor = append(r.approvedFor, input["org_id"].(string))
		return map[string]any{"redirect_url": "http://127.0.0.1/callback?code=code-1&state=e2e"}, nil
	}
	return nil, &CallError{Code: "NOT_FOUND"}
}

// The MCP caller turns the scenario's session into an MCP access token
// through the authorization server, once per session, and presents it on
// initialize and every call; a call without a token is UNAUTHENTICATED.
func TestMCPCaller_OAuthExchange(t *testing.T) {
	var registered, tokens int
	as := http.NewServeMux()
	as.HandleFunc("/oauth/register", func(w http.ResponseWriter, _ *http.Request) {
		registered++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"c-1"}`))
	})
	as.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resource") != "https://mcp.example/mcp" || r.URL.Query().Get("client_id") != "c-1" {
			http.Error(w, "bad", 400)
			return
		}
		http.Redirect(w, r, "http://ui.example/authorize?request=req-1", http.StatusFound)
	})
	as.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "code-1" || r.PostForm.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		tokens++
		_, _ = w.Write([]byte(`{"access_token":"mcp-token","token_type":"Bearer"}`))
	})
	asSrv := httptest.NewServer(as)
	defer asSrv.Close()

	mcp := http.NewServeMux()
	mcp.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resource":"https://mcp.example/mcp"}`))
	})
	mcp.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-token" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="x"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var rpc map[string]any
		_ = json.Unmarshal(body, &rpc)
		w.Header().Set(mcpSessionHeader, "s-1")
		switch rpc["method"] {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "tools/call":
			if rpc["params"].(map[string]any)["name"] == "missing" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"NotFound: Not found."}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	mcpSrv := httptest.NewServer(mcp)
	defer mcpSrv.Close()

	rest := &restFake{}
	get := Endpoint{Ref: "auth.AuthService.GetOAuthAuthorization"}
	approve := Endpoint{Ref: "auth.AuthService.ApproveOAuthAuthorization"}
	c := NewMCPCaller(mcpSrv.URL+"/mcp", nil).WithOAuth(MCPOAuth{Issuer: asSrv.URL, REST: rest, Get: &get, Approve: &approve})

	for range 2 {
		got, err := c.Call(context.Background(), Endpoint{Ref: "app.X.Y", ToolName: "y"}, map[string]any{}, "sess", nil, "", nil)
		if err != nil || got["ok"] != true {
			t.Fatalf("call: %v %v", got, err)
		}
	}
	if registered != 1 || tokens != 1 || len(rest.approvedFor) != 1 || rest.approvedFor[0] != "org-1" {
		t.Errorf("registered %d, tokens %d, approvals %v — want one each, for org-1", registered, tokens, rest.approvedFor)
	}

	_, err := c.Call(context.Background(), Endpoint{Ref: "app.X.Missing", ToolName: "missing"}, map[string]any{}, "sess", nil, "", nil)
	if ce, ok := err.(*CallError); !ok || ce.Code != "NOT_FOUND" {
		t.Errorf("an isError result: %v, want a NOT_FOUND CallError", err)
	}

	fresh := NewMCPCaller(mcpSrv.URL+"/mcp", nil).WithOAuth(MCPOAuth{Issuer: asSrv.URL, REST: rest, Approve: &approve})
	_, err = fresh.Call(context.Background(), Endpoint{Ref: "app.X.Y", ToolName: "y"}, map[string]any{}, "", nil, "", nil)
	if ce, ok := err.(*CallError); !ok || ce.Code != "UNAUTHENTICATED" {
		t.Errorf("no token: %v, want UNAUTHENTICATED", err)
	}
}
