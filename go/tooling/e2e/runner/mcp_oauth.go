package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// An MCP surface that is an OAuth 2.1 protected resource ((w17.mcp_api).oauth)
// takes only an access token its authorization server issued for it — never
// the REST session a scenario signed in with. The runner gets one the way an
// MCP client does, with the scenario's session standing in for the user's
// browser: register a client, start an authorization, approve it on the
// consent RPC with the session (signed with its device, when the surface
// binds devices), and redeem the code. One token per session, kept for the
// run.

// MCPOAuth configures that exchange (MCPCaller.WithOAuth).
type MCPOAuth struct {
	// Issuer is the authorization server's base URL as the runner reaches
	// it: the REST gateway's root (the OAuth endpoints are served there,
	// outside any API prefix).
	Issuer string
	// REST is the caller the consent RPCs go through — the scenario's
	// REST caller, signing with the session's device.
	REST Caller
	// Get and Approve are the consent page's RPCs (GetOAuthAuthorization,
	// ApproveOAuthAuthorization) as the REST table publishes them. Get is
	// optional: without it no organization is chosen.
	Get, Approve *Endpoint
	// Client is the HTTP client for the OAuth endpoints (nil: default).
	Client *http.Client
}

// WithOAuth makes the caller exchange each step's session token for an MCP
// access token through o before calling the surface.
func (c *MCPCaller) WithOAuth(o MCPOAuth) *MCPCaller {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	c.oauth = &mcpOAuthState{cfg: o, tokens: map[string]string{}}
	return c
}

type mcpOAuthState struct {
	cfg MCPOAuth

	mu       sync.Mutex
	clientID string
	resource string
	tokens   map[string]string // session token → MCP access token
}

const e2eRedirect = "http://127.0.0.1/callback"

// accessToken is the MCP token for session, obtained on first use.
func (o *mcpOAuthState) accessToken(ctx context.Context, endpoint, session string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t, ok := o.tokens[session]; ok {
		return t, nil
	}
	if o.resource == "" {
		res, err := o.discoverResource(ctx, endpoint)
		if err != nil {
			return "", err
		}
		o.resource = res
	}
	if o.clientID == "" {
		id, err := o.register(ctx)
		if err != nil {
			return "", err
		}
		o.clientID = id
	}
	tok, err := o.authorize(ctx, session)
	if err != nil {
		return "", err
	}
	o.tokens[session] = tok
	return tok, nil
}

// discoverResource reads the surface's protected-resource metadata: the
// resource it names is what the token is issued for.
func (o *mcpOAuthState) discoverResource(ctx context.Context, endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	u.Path = "/.well-known/oauth-protected-resource" + strings.TrimSuffix(u.Path, "/")
	var doc struct {
		Resource string `json:"resource"`
	}
	if err := o.getJSON(ctx, u.String(), &doc); err != nil {
		return "", fmt.Errorf("mcp oauth: protected-resource metadata: %w", err)
	}
	if doc.Resource == "" {
		return "", fmt.Errorf("mcp oauth: %s names no resource", u)
	}
	return doc.Resource, nil
}

func (o *mcpOAuthState) register(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{e2eRedirect},
		"client_name":   "w17 e2e",
		"grant_types":   []string{"authorization_code", "refresh_token"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.Issuer+"/oauth/register", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := o.doJSON(req, http.StatusCreated, &out); err != nil {
		return "", fmt.Errorf("mcp oauth: register: %w", err)
	}
	return out.ClientID, nil
}

// authorize runs one authorization for session: the authorization endpoint,
// the consent RPCs with the session, the code exchange.
func (o *mcpOAuthState) authorize(ctx context.Context, session string) (string, error) {
	verifierRaw := make([]byte, 32)
	if _, err := rand.Read(verifierRaw); err != nil {
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {o.clientID}, "redirect_uri": {e2eRedirect},
		"state": {"e2e"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}, "resource": {o.resource}, "scope": {"mcp"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cfg.Issuer+"/oauth/authorize?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	noFollow := *o.cfg.Client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: authorize: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("request") == "" {
		return "", fmt.Errorf("mcp oauth: authorize: status %d, Location %q: %s", resp.StatusCode, resp.Header.Get("Location"), truncate(raw, 256))
	}
	requestID := loc.Query().Get("request")

	org := ""
	if o.cfg.Get != nil {
		got, err := o.cfg.REST.Call(ctx, *o.cfg.Get, map[string]any{"request_id": requestID}, session, nil, "", nil)
		if err != nil {
			return "", fmt.Errorf("mcp oauth: consent read: %w", err)
		}
		if orgs, _ := got["orgs"].([]any); len(orgs) > 0 {
			if first, ok := orgs[0].(map[string]any); ok {
				org, _ = first["id"].(string)
			}
		}
	}
	approved, err := o.cfg.REST.Call(ctx, *o.cfg.Approve, map[string]any{"request_id": requestID, "org_id": org}, session, nil, "", nil)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: approve: %w", err)
	}
	redirect, _ := approved["redirect_url"].(string)
	if redirect == "" {
		redirect, _ = approved["redirectUrl"].(string)
	}
	ru, err := url.Parse(redirect)
	if err != nil || ru.Query().Get("code") == "" {
		return "", fmt.Errorf("mcp oauth: approve answered no code: %v", approved)
	}

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {ru.Query().Get("code")}, "code_verifier": {verifier},
		"client_id": {o.clientID}, "redirect_uri": {e2eRedirect},
	}
	treq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.Issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	treq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := o.doJSON(treq, http.StatusOK, &tok); err != nil {
		return "", fmt.Errorf("mcp oauth: token: %w", err)
	}
	return tok.AccessToken, nil
}

func (o *mcpOAuthState) getJSON(ctx context.Context, target string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	return o.doJSON(req, http.StatusOK, v)
}

func (o *mcpOAuthState) doJSON(req *http.Request, want int, v any) error {
	resp, err := o.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s: status %d: %s", req.Method, req.URL, resp.StatusCode, truncate(raw, 256))
	}
	return json.Unmarshal(raw, v)
}
