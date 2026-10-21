package oauthas

import (
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// singleValued are the parameters that may appear once (RFC 6749 §3.1,
// §3.2): a second copy is a request someone built to be read two ways.
var singleValued = []string{
	"response_type", "client_id", "redirect_uri", "state", "scope",
	"code_challenge", "code_challenge_method", "grant_type", "code",
	"code_verifier", "refresh_token", "token", "token_type_hint",
}

func duplicated(v url.Values) string {
	for _, k := range singleValued {
		if len(v[k]) > 1 {
			return k
		}
	}
	return ""
}

// readForm parses a form-encoded POST body (the token and revocation
// endpoints, RFC 6749 §3.2) and nothing else: parameters in the query of a
// token request would put a code or a refresh token in access logs.
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if r.Method != http.MethodPost {
		return nil, &Error{Code: "invalid_request", Description: "use POST"}
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-www-form-urlencoded" {
		return nil, &Error{Code: "invalid_request", Description: "the body must be application/x-www-form-urlencoded"}
	}
	if r.URL.RawQuery != "" {
		return nil, &Error{Code: "invalid_request", Description: "parameters belong in the body, not the query"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return nil, &Error{Code: "invalid_request", Description: "the body is not a valid form"}
	}
	if k := duplicated(r.PostForm); k != "" {
		return nil, &Error{Code: "invalid_request", Description: "parameter " + k + " is repeated"}
	}
	return r.PostForm, nil
}

// authorize starts an authorization: the backend checks the request and
// keeps it pending, and the browser goes to the consent page. An error the
// client may hear goes back to its redirect URI; one about the client or
// the redirect itself is shown to the user (RFC 6749 §4.1.2.1).
func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	var q url.Values
	switch r.Method {
	case http.MethodGet:
		q = r.URL.Query()
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := r.ParseForm(); err != nil {
			errorPage(w, &Error{Code: "invalid_request", Description: "the request is not a valid form"})
			return
		}
		q = r.Form
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if k := duplicated(q); k != "" {
		errorPage(w, &Error{Code: "invalid_request", Description: "parameter " + k + " is repeated"})
		return
	}
	if !s.authLimit.AllowRequest(r) {
		w.Header().Set("Retry-After", "60")
		errorPage(w, &Error{Code: "temporarily_unavailable", Description: "too many sign-in requests from this address; wait a minute"})
		return
	}
	req := AuthorizeRequest{
		ResponseType:        q.Get("response_type"),
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		State:               q.Get("state"),
		Scope:               q.Get("scope"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Resources:           q["resource"],
		Issuer:              s.cfg.Issuer,
		ClientAddr:          s.clientAddr(r),
	}
	res, err := s.b.Authorize(r.Context(), req)
	if err != nil {
		var oe *Error
		if !errors.As(err, &oe) {
			errorPage(w, &Error{Code: "temporarily_unavailable", Description: "the sign-in service is unavailable; try again"})
			return
		}
		if oe.NoRedirect || req.RedirectURI == "" {
			errorPage(w, oe)
			return
		}
		s.redirectError(w, r, req, oe)
		return
	}
	to := *s.consent
	cq := to.Query()
	cq.Set("request", res.RequestID)
	to.RawQuery = cq.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, to.String(), http.StatusFound)
}

// redirectError sends an authorization error to the client's redirect URI,
// which the backend has already matched (it only answers a redirectable
// error for a registered redirect).
func (s *server) redirectError(w http.ResponseWriter, r *http.Request, req AuthorizeRequest, oe *Error) {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		errorPage(w, oe)
		return
	}
	q := u.Query()
	q.Set("error", oe.Code)
	if oe.Description != "" {
		q.Set("error_description", oe.Description)
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.cfg.Issuer)
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// errorPage shows an authorization error to the user.
func errorPage(w http.ResponseWriter, oe *Error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Sign-in request refused</title>` +
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">` +
		`<h1 style="font-size:1.25rem">This sign-in request cannot be completed</h1><p>` +
		html.EscapeString(oe.Description) + `</p><p style="color:dimgray">Error: ` + html.EscapeString(oe.Code) +
		`</p><p>Close this window and start again from the application.</p></body>`))
}

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	if cors(w, r, "POST") {
		return
	}
	ok, done := s.tokLimit.ReserveRequest(r)
	if !ok {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
		return
	}
	failed := true
	defer func() { done(failed) }()

	form, err := readForm(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	req := TokenRequest{
		GrantType:    form.Get("grant_type"),
		Code:         form.Get("code"),
		RedirectURI:  form.Get("redirect_uri"),
		CodeVerifier: form.Get("code_verifier"),
		ClientID:     form.Get("client_id"),
		RefreshToken: form.Get("refresh_token"),
		Resources:    form["resource"],
		Scope:        form.Get("scope"),
		ClientAddr:   s.clientAddr(r),
		UserAgent:    r.UserAgent(),
	}
	if req.ClientID == "" {
		writeError(w, &Error{Code: "invalid_client", Description: "client_id is required"})
		return
	}
	if len(r.Header.Values(headerDPoP)) > 0 {
		if s.cfg.DPoP == nil {
			writeError(w, &Error{Code: "invalid_dpop_proof", Description: "this server does not take DPoP proofs"})
			return
		}
		thumb, nonce, err := s.cfg.DPoP(r)
		if nonce != "" {
			w.Header().Set(headerDPoPNonce, nonce)
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrDPoPUseNonce):
				writeError(w, &Error{Code: "use_dpop_nonce", Description: "a fresh DPoP nonce is required"})
			case errors.Is(err, ErrDPoPUnavailable):
				w.Header().Set("Retry-After", "1")
				writeError(w, err)
			case errors.Is(err, ErrDPoPSlowDown):
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
			default:
				writeError(w, &Error{Code: "invalid_dpop_proof", Description: "the DPoP proof was refused"})
			}
			return
		}
		req.DPoPThumbprint = thumb
	}
	resp, err := s.b.Token(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	failed = false
	body := map[string]any{
		"access_token": resp.AccessToken,
		"token_type":   resp.TokenType,
	}
	if resp.ExpiresIn > 0 {
		body["expires_in"] = resp.ExpiresIn
	}
	if resp.RefreshToken != "" {
		body["refresh_token"] = resp.RefreshToken
	}
	if resp.Scope != "" {
		body["scope"] = resp.Scope
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	if cors(w, r, "POST") {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	st, err := s.b.Settings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if !st.DynamicRegistration {
		http.NotFound(w, r)
		return
	}
	if !s.regLimit.AllowRequest(r) {
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, &Error{Code: "invalid_client_metadata", Description: "the body must be application/json"})
		return
	}
	var md ClientMetadata
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&md); err != nil {
		writeError(w, &Error{Code: "invalid_client_metadata", Description: "the body is not valid client metadata"})
		return
	}
	if m := md.TokenEndpointAuthMethod; m != "" && m != "none" {
		writeError(w, &Error{Code: "invalid_client_metadata", Description: "only public clients register here (token_endpoint_auth_method none)"})
		return
	}
	md.TokenEndpointAuthMethod = "none"
	if len(md.GrantTypes) == 0 {
		md.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(md.ResponseTypes) == 0 {
		md.ResponseTypes = []string{"code"}
	}
	rc, err := s.b.Register(r.Context(), md, s.clientAddr(r))
	if err != nil {
		writeError(w, err)
		return
	}
	body := map[string]any{
		"client_id":                  rc.ClientID,
		"client_id_issued_at":        rc.IssuedAt.Unix(),
		"redirect_uris":              md.RedirectURIs,
		"token_endpoint_auth_method": md.TokenEndpointAuthMethod,
		"grant_types":                md.GrantTypes,
		"response_types":             md.ResponseTypes,
	}
	for k, v := range map[string]string{"client_name": md.ClientName, "client_uri": md.ClientURI, "logo_uri": md.LogoURI,
		"software_id": md.SoftwareID, "software_version": md.SoftwareVersion, "scope": md.Scope} {
		if v != "" {
			body[k] = v
		}
	}
	writeJSON(w, http.StatusCreated, body)
}

func (s *server) revoke(w http.ResponseWriter, r *http.Request) {
	if cors(w, r, "POST") {
		return
	}
	if !s.tokLimit.AllowRequest(r) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
		return
	}
	form, err := readForm(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	token := strings.TrimSpace(form.Get("token"))
	if token == "" {
		writeError(w, &Error{Code: "invalid_request", Description: "token is required"})
		return
	}
	if err := s.b.Revoke(r.Context(), token, form.Get("token_type_hint"), form.Get("client_id"), s.clientAddr(r)); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}
