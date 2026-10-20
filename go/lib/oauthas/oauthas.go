// Package oauthas is the OAuth 2.1 wire of an authorization server: the HTTP
// protocol MCP clients and native apps speak (RFC 8414 metadata, the
// authorization and token endpoints, RFC 7591 registration, RFC 7009
// revocation), turned into calls of a [Backend].
//
// It holds no state and decides nothing about clients, codes or tokens —
// the auth plugin's `oauth_server` feature does, behind the generated
// Backend the REST gateway mounts this with. What lives here is the part a
// REST route cannot express: form-encoded bodies, the OAuth error JSON, a
// 302 to the client's own redirect with the code in the query, and paths at
// the root of the issuer, outside any API prefix.
package oauthas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// The endpoint paths, at the root of the issuer.
const (
	MetadataPath  = "/.well-known/oauth-authorization-server"
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
	RegisterPath  = "/oauth/register"
	RevokePath    = "/oauth/revoke"
)

// Error is an OAuth error (RFC 6749 §5.2, RFC 7591 §3.2.2): Code is the
// registered error code (`invalid_grant`, `invalid_client_metadata`, …).
// A Backend returns one for every refusal it decides; any other error is a
// failure, answered as `server_error` / `temporarily_unavailable`.
type Error struct {
	Code        string
	Description string
	// NoRedirect marks an authorization error that must NOT be sent to the
	// redirect URI: the client or the redirect itself is what failed to
	// check (RFC 6749 §4.1.2.1), so the user is shown the error instead.
	NoRedirect bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

// Settings are what the metadata document needs from the backend.
type Settings struct {
	// DynamicRegistration publishes and serves the registration endpoint.
	DynamicRegistration bool
}

// ClientMetadata is an RFC 7591 registration request.
type ClientMetadata struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// RegisteredClient is the backend's answer to a registration.
type RegisteredClient struct {
	ClientID string
	IssuedAt time.Time
}

// AuthorizeRequest is an authorization request (RFC 6749 §4.1.1 with PKCE,
// RFC 8707 resource).
type AuthorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	// Resources are every `resource` parameter sent (RFC 8707 allows
	// several; the backend decides how many it takes).
	Resources []string
	// Issuer is this server's issuer, recorded so the redirect carries it
	// (RFC 9207 `iss`).
	Issuer     string
	ClientAddr string
}

// AuthorizeResult is the pending request the consent page decides.
type AuthorizeResult struct{ RequestID string }

// TokenRequest is a token request (RFC 6749 §4.1.3, §6).
type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	CodeVerifier string
	ClientID     string
	RefreshToken string
	Resources    []string
	Scope        string
	// DPoPThumbprint is the key the request's DPoP proof was verified with
	// (RFC 9449 §5); empty when it carried none.
	DPoPThumbprint string
	ClientAddr     string
	// UserAgent is the request's, for the sign-in history.
	UserAgent string
}

// TokenResponse is a successful token response (RFC 6749 §5.1).
type TokenResponse struct {
	AccessToken string
	// TokenType is "Bearer" or "DPoP".
	TokenType    string
	ExpiresIn    int64
	RefreshToken string
	Scope        string
}

// Backend is the authorization server's logic.
type Backend interface {
	Settings(ctx context.Context) (Settings, error)
	Register(ctx context.Context, md ClientMetadata, clientAddr string) (RegisteredClient, error)
	Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResult, error)
	Token(ctx context.Context, req TokenRequest) (TokenResponse, error)
	// Revoke revokes the grant behind token; an unknown token is not an
	// error (RFC 7009 §2.2).
	Revoke(ctx context.Context, token, hint, clientID, clientAddr string) error
}

// DPoPFunc verifies the DPoP proof of a token request. It returns the
// proof key's thumbprint, "" when the request carries no proof, and a
// lib/dpop error for a proof it refuses. nonce, when non-empty, is sent
// back as DPoP-Nonce for the client's next proof.
type DPoPFunc func(r *http.Request) (thumbprint, nonce string, err error)

// Config configures Mount.
type Config struct {
	// Issuer is the server's issuer identifier: an https URL with no query,
	// fragment or trailing slash, where the paths above are served.
	Issuer string
	// ConsentURL is the page the user decides on; `request=<id>` is added.
	ConsentURL string
	// TrustedProxies are believed for the client address (rate limits, the
	// backend's records).
	TrustedProxies []netip.Prefix
	// DPoP verifies proofs on the token endpoint. Nil: a request carrying
	// a proof is refused (`invalid_dpop_proof`), and no session-bound grant
	// can be redeemed.
	DPoP DPoPFunc
	// RegisterPerHour caps registrations per client address (default 20;
	// negative = no cap).
	RegisterPerHour int
	// TokenPerMinute caps token requests per client address (default 60;
	// negative = no cap).
	TokenPerMinute int
}

// Router is what Mount registers on (chi.Router and http.ServeMux both are).
type Router interface {
	Handle(pattern string, h http.Handler)
}

// Mount registers the endpoints on r. It refuses a configuration that
// would serve a broken flow.
func Mount(r Router, cfg Config, b Backend) error {
	s, err := newServer(cfg, b)
	if err != nil {
		return err
	}
	r.Handle(MetadataPath, http.HandlerFunc(s.metadata))
	r.Handle(AuthorizePath, http.HandlerFunc(s.authorize))
	r.Handle(TokenPath, http.HandlerFunc(s.token))
	r.Handle(RegisterPath, http.HandlerFunc(s.register))
	r.Handle(RevokePath, http.HandlerFunc(s.revoke))
	return nil
}

type server struct {
	cfg      Config
	b        Backend
	consent  *url.URL
	regLimit *ratelimit.Limiter
	tokLimit *ratelimit.Limiter
}

func newServer(cfg Config, b Backend) (*server, error) {
	if b == nil {
		return nil, errors.New("oauthas: no backend")
	}
	iss, err := url.Parse(cfg.Issuer)
	if err != nil || !iss.IsAbs() || iss.Host == "" || iss.RawQuery != "" || iss.Fragment != "" || strings.HasSuffix(cfg.Issuer, "/") {
		return nil, fmt.Errorf("oauthas: issuer %q is not an absolute URL without query, fragment or trailing slash", cfg.Issuer)
	}
	if iss.Scheme != "https" && !loopbackHost(iss.Hostname()) {
		return nil, fmt.Errorf("oauthas: issuer %q must be https (http only on a loopback host)", cfg.Issuer)
	}
	consent, err := url.Parse(cfg.ConsentURL)
	if err != nil || !consent.IsAbs() || consent.Host == "" || (consent.Scheme != "https" && !loopbackHost(consent.Hostname())) {
		return nil, fmt.Errorf("oauthas: consent URL %q is not an https URL", cfg.ConsentURL)
	}
	s := &server{cfg: cfg, b: b, consent: consent}
	if n := orDefault(cfg.RegisterPerHour, 20); n > 0 {
		s.regLimit = ratelimit.New(ratelimit.Config{PerMinute: float64(n) / 60, Burst: min(n, 5), TrustedProxies: cfg.TrustedProxies})
	}
	if n := orDefault(cfg.TokenPerMinute, 60); n > 0 {
		s.tokLimit = ratelimit.New(ratelimit.Config{PerMinute: float64(n), Burst: min(n, 20), TrustedProxies: cfg.TrustedProxies})
	}
	return s, nil
}

func orDefault(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

func (s *server) clientAddr(r *http.Request) string {
	return ratelimit.HTTPClientAddr(r, s.cfg.TrustedProxies)
}

// maxBody bounds every request body: the largest legitimate one is a
// registration with a handful of redirect URIs.
const maxBody = 64 << 10

// cors lets a browser-based client read the JSON endpoints. No credentials
// travel by cookie here, so a wildcard origin grants nothing.
func cors(w http.ResponseWriter, r *http.Request, methods string) (preflight bool) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", methods)
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, DPoP")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	h.Set("Access-Control-Expose-Headers", "DPoP-Nonce, WWW-Authenticate")
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers an OAuth error. A backend failure that is not an
// *Error is the server's, and its text is not the client's to read.
func writeError(w http.ResponseWriter, err error) {
	var oe *Error
	if !errors.As(err, &oe) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	code := http.StatusBadRequest
	if oe.Code == "invalid_client" {
		code = http.StatusUnauthorized
	}
	body := map[string]string{"error": oe.Code}
	if oe.Description != "" {
		body["error_description"] = oe.Description
	}
	writeJSON(w, code, body)
}

func (s *server) endpoint(p string) string { return s.cfg.Issuer + p }

func (s *server) metadata(w http.ResponseWriter, r *http.Request) {
	if cors(w, r, "GET") {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	st, err := s.b.Settings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	doc := map[string]any{
		"issuer":                                s.cfg.Issuer,
		"authorization_endpoint":                s.endpoint(AuthorizePath),
		"token_endpoint":                        s.endpoint(TokenPath),
		"revocation_endpoint":                   s.endpoint(RevokePath),
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                              []string{"mcp"},
		"client_id_metadata_document_supported":         true,
		"authorization_response_iss_parameter_supported": true,
	}
	if st.DynamicRegistration {
		doc["registration_endpoint"] = s.endpoint(RegisterPath)
	}
	if s.cfg.DPoP != nil {
		doc["dpop_signing_alg_values_supported"] = []string{"ES256", "EdDSA"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=300")
	_ = json.NewEncoder(w).Encode(doc)
}
