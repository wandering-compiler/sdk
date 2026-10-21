package oauthas

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	settings  Settings
	authReq   AuthorizeRequest
	authErr   error
	tokenReq  TokenRequest
	tokenErr  error
	regMD     ClientMetadata
	revoked   string
	revokeErr error
}

func (f *fakeBackend) Settings(context.Context) (Settings, error) { return f.settings, nil }
func (f *fakeBackend) Register(_ context.Context, md ClientMetadata, _ string) (RegisteredClient, error) {
	f.regMD = md
	return RegisteredClient{ClientID: "c-1", IssuedAt: time.Unix(1700000000, 0)}, nil
}
func (f *fakeBackend) Authorize(_ context.Context, req AuthorizeRequest) (AuthorizeResult, error) {
	f.authReq = req
	if f.authErr != nil {
		return AuthorizeResult{}, f.authErr
	}
	return AuthorizeResult{RequestID: "req-1"}, nil
}
func (f *fakeBackend) Token(_ context.Context, req TokenRequest) (TokenResponse, error) {
	f.tokenReq = req
	if f.tokenErr != nil {
		return TokenResponse{}, f.tokenErr
	}
	return TokenResponse{AccessToken: "at", TokenType: "Bearer", ExpiresIn: 3600, RefreshToken: "rt", Scope: "mcp"}, nil
}
func (f *fakeBackend) Revoke(_ context.Context, token, _, _, _ string) error {
	f.revoked = token
	return f.revokeErr
}

func mount(t *testing.T, cfg Config, b Backend) http.Handler {
	t.Helper()
	if cfg.Issuer == "" {
		cfg.Issuer = "https://auth.example.com"
	}
	if cfg.ConsentURL == "" {
		cfg.ConsentURL = "https://app.example.com/authorize?x=1"
	}
	mux := http.NewServeMux()
	if err := Mount(mux, cfg, b); err != nil {
		t.Fatal(err)
	}
	return mux
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func form(path string, v url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestMount_RefusesABrokenConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{Issuer: "", ConsentURL: "https://a.example/c"},
		{Issuer: "https://a.example/", ConsentURL: "https://a.example/c"},
		{Issuer: "http://a.example", ConsentURL: "https://a.example/c"},
		{Issuer: "https://a.example?x", ConsentURL: "https://a.example/c"},
		{Issuer: "https://a.example", ConsentURL: "/consent"},
		{Issuer: "https://a.example", ConsentURL: "http://a.example/c"},
		{Issuer: "https://a.example/auth", ConsentURL: "https://a.example/c"},
	} {
		if err := Mount(http.NewServeMux(), cfg, &fakeBackend{}); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	if err := Mount(http.NewServeMux(), Config{Issuer: "http://localhost:8080", ConsentURL: "http://127.0.0.1:3000/a"}, &fakeBackend{}); err != nil {
		t.Errorf("loopback dev configuration refused: %v", err)
	}
}

func TestMetadata(t *testing.T) {
	h := mount(t, Config{}, &fakeBackend{settings: Settings{DynamicRegistration: true}})
	w := do(h, httptest.NewRequest(http.MethodGet, MetadataPath, nil))
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for k, want := range map[string]any{
		"issuer":                                "https://auth.example.com",
		"authorization_endpoint":                "https://auth.example.com/oauth/authorize",
		"token_endpoint":                        "https://auth.example.com/oauth/token",
		"registration_endpoint":                 "https://auth.example.com/oauth/register",
		"revocation_endpoint":                   "https://auth.example.com/oauth/revoke",
		"client_id_metadata_document_supported": true,
	} {
		if doc[k] != want {
			t.Errorf("%s = %v, want %v", k, doc[k], want)
		}
	}
	if m := doc["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", m)
	}
	if _, ok := doc["dpop_signing_alg_values_supported"]; ok {
		t.Error("DPoP advertised with no verifier")
	}

	h = mount(t, Config{}, &fakeBackend{})
	w = do(h, httptest.NewRequest(http.MethodGet, MetadataPath, nil))
	if strings.Contains(w.Body.String(), "registration_endpoint") {
		t.Error("registration advertised while off")
	}
	if w := do(h, httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(`{}`))); w.Code != http.StatusNotFound {
		t.Errorf("registration while off: %d", w.Code)
	}
}

func TestAuthorize_GoesToTheConsentPage(t *testing.T) {
	b := &fakeBackend{}
	h := mount(t, Config{}, b)
	q := url.Values{"response_type": {"code"}, "client_id": {"https://claude.ai/oauth/client.json"},
		"redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}, "state": {"s1"}, "code_challenge": {"ch"},
		"code_challenge_method": {"S256"}, "resource": {"https://mcp.example.com/mcp"}, "scope": {"mcp"}}
	w := do(h, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://app.example.com/authorize?request=req-1&x=1" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	if b.authReq.Issuer != "https://auth.example.com" || b.authReq.Resources[0] != "https://mcp.example.com/mcp" || b.authReq.CodeChallenge != "ch" {
		t.Errorf("backend got %+v", b.authReq)
	}
}

func TestAuthorize_Errors(t *testing.T) {
	base := url.Values{"response_type": {"code"}, "client_id": {"c"}, "redirect_uri": {"https://c.example/cb"}, "state": {"s1"}}
	// A redirectable error goes to the client with state and iss.
	h := mount(t, Config{}, &fakeBackend{authErr: &Error{Code: "invalid_request", Description: "PKCE is required"}})
	w := do(h, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+base.Encode(), nil))
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || loc.Host != "c.example" || loc.Query().Get("error") != "invalid_request" ||
		loc.Query().Get("state") != "s1" || loc.Query().Get("iss") != "https://auth.example.com" {
		t.Errorf("%d %v", w.Code, loc)
	}
	// An error about the client or redirect is shown, never redirected.
	h = mount(t, Config{}, &fakeBackend{authErr: &Error{Code: "invalid_request", Description: "unknown <client>", NoRedirect: true}})
	w = do(h, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+base.Encode(), nil))
	if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "unknown &lt;client&gt;") {
		t.Errorf("%d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	// A backend failure is shown, never redirected.
	h = mount(t, Config{}, &fakeBackend{authErr: errors.New("db down")})
	w = do(h, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+base.Encode(), nil))
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "db down") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	// A repeated parameter is refused before the backend sees it.
	b := &fakeBackend{}
	h = mount(t, Config{}, b)
	w = do(h, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+base.Encode()+"&redirect_uri=https://evil.example/cb", nil))
	if w.Code != http.StatusBadRequest || b.authReq.ClientID != "" {
		t.Errorf("repeated redirect_uri: %d, backend asked: %v", w.Code, b.authReq.ClientID != "")
	}
}

func TestToken(t *testing.T) {
	b := &fakeBackend{}
	h := mount(t, Config{}, b)
	w := do(h, form(TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "code_verifier": {"v"},
		"client_id": {"c-1"}, "redirect_uri": {"https://c.example/cb"}, "resource": {"https://mcp.example.com/mcp"}}))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || body["access_token"] != "at" || body["refresh_token"] != "rt" || body["token_type"] != "Bearer" ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if b.tokenReq.Code != "c" || b.tokenReq.CodeVerifier != "v" || b.tokenReq.DPoPThumbprint != "" {
		t.Errorf("backend got %+v", b.tokenReq)
	}

	cases := []struct {
		name string
		r    *http.Request
		code int
		err  string
	}{
		{"query params", func() *http.Request {
			r := form(TokenPath+"?code=x", url.Values{"client_id": {"c"}})
			return r
		}(), 400, "invalid_request"},
		{"json body", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			return r
		}(), 400, "invalid_request"},
		{"no client", form(TokenPath, url.Values{"grant_type": {"authorization_code"}}), 401, "invalid_client"},
		{"GET", httptest.NewRequest(http.MethodGet, TokenPath, nil), 400, "invalid_request"},
	}
	for _, c := range cases {
		w := do(h, c.r)
		if w.Code != c.code || !strings.Contains(w.Body.String(), `"error":"`+c.err+`"`) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}

	h = mount(t, Config{}, &fakeBackend{tokenErr: &Error{Code: "invalid_grant", Description: "the code is spent"}})
	w = do(h, form(TokenPath, url.Values{"grant_type": {"authorization_code"}, "client_id": {"c"}}))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"error":"invalid_grant"`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	h = mount(t, Config{}, &fakeBackend{tokenErr: errors.New("secret db detail")})
	w = do(h, form(TokenPath, url.Values{"grant_type": {"authorization_code"}, "client_id": {"c"}}))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// A token request with a proof is verified; its key reaches the backend; a
// nonce demand is answered as RFC 9449 §8 says. No verifier = refused.
func TestToken_DPoP(t *testing.T) {
	v := url.Values{"grant_type": {"authorization_code"}, "client_id": {"c"}}
	withProof := func() *http.Request {
		r := form(TokenPath, v)
		r.Header.Set("DPoP", "proof")
		return r
	}
	h := mount(t, Config{}, &fakeBackend{})
	if w := do(h, withProof()); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_dpop_proof") {
		t.Errorf("no verifier: %d %s", w.Code, w.Body)
	}

	b := &fakeBackend{}
	h = mount(t, Config{DPoP: func(*http.Request) (string, string, error) { return "thumb", "n2", nil }}, b)
	w := do(h, withProof())
	if w.Code != 200 || b.tokenReq.DPoPThumbprint != "thumb" || w.Header().Get("DPoP-Nonce") != "n2" {
		t.Errorf("%d %s %+v", w.Code, w.Body, b.tokenReq)
	}

	h = mount(t, Config{DPoP: func(*http.Request) (string, string, error) {
		return "", "n1", ErrDPoPUseNonce
	}}, &fakeBackend{})
	w = do(h, withProof())
	if w.Code != 400 || !strings.Contains(w.Body.String(), "use_dpop_nonce") || w.Header().Get("DPoP-Nonce") != "n1" {
		t.Errorf("nonce: %d %s %v", w.Code, w.Body, w.Header())
	}

	// Over the verifier's budget is the endpoint's rate limit, not a refused
	// proof: a client must not take its key for a bad one.
	h = mount(t, Config{DPoP: func(*http.Request) (string, string, error) {
		return "", "", ErrDPoPSlowDown
	}}, &fakeBackend{})
	w = do(h, withProof())
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "slow_down") || w.Header().Get("Retry-After") == "" {
		t.Errorf("slow down: %d %s %v", w.Code, w.Body, w.Header())
	}
}

func TestRegister(t *testing.T) {
	b := &fakeBackend{settings: Settings{DynamicRegistration: true}}
	h := mount(t, Config{}, b)
	r := httptest.NewRequest(http.MethodPost, RegisterPath,
		strings.NewReader(`{"redirect_uris":["http://127.0.0.1:33418/callback"],"client_name":"Claude Code"}`))
	r.Header.Set("Content-Type", "application/json")
	w := do(h, r)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"client_id":"c-1"`) ||
		b.regMD.ClientName != "Claude Code" || b.regMD.TokenEndpointAuthMethod != "none" {
		t.Fatalf("%d %s %+v", w.Code, w.Body, b.regMD)
	}
	r = httptest.NewRequest(http.MethodPost, RegisterPath,
		strings.NewReader(`{"redirect_uris":["https://c.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`))
	r.Header.Set("Content-Type", "application/json")
	if w := do(h, r); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_client_metadata") {
		t.Errorf("confidential client: %d %s", w.Code, w.Body)
	}
}

// Registration is capped per address.
func TestRegister_RateLimited(t *testing.T) {
	h := mount(t, Config{RegisterPerHour: 2}, &fakeBackend{settings: Settings{DynamicRegistration: true}})
	var last int
	for range 3 {
		r := httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(`{"redirect_uris":["https://c.example/cb"]}`))
		r.Header.Set("Content-Type", "application/json")
		last = do(h, r).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("third registration: %d", last)
	}
}

func TestRevoke(t *testing.T) {
	b := &fakeBackend{}
	h := mount(t, Config{}, b)
	if w := do(h, form(RevokePath, url.Values{"token": {"rt"}, "client_id": {"c"}})); w.Code != 200 || b.revoked != "rt" {
		t.Errorf("%d %q", w.Code, b.revoked)
	}
	if w := do(h, form(RevokePath, url.Values{})); w.Code != 400 {
		t.Errorf("no token: %d", w.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := mount(t, Config{}, &fakeBackend{})
	w := do(h, httptest.NewRequest(http.MethodOptions, TokenPath, nil))
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "DPoP") {
		t.Errorf("%d %v", w.Code, w.Header())
	}
}
