package oauthas

import (
	"html"
	"net/http"
)

// CodePath is the code page: the redirect of a program that cannot receive
// one (w17ctl on another machine than the browser — an ssh session, a remote
// workspace). Instead of a 302 to the program, the browser lands here and is
// shown the answer to type into it. The program sends CodeRedirectURI as its
// redirect_uri, and the authorization server decides whether such a request
// may proceed (the auth plugin: only w17ctl, and only when the deployment
// names this page in oauth_code_redirect).
const CodePath = "/oauth/code"

// CodeRedirectURI is the code page of the server at issuer.
func CodeRedirectURI(issuer string) string { return issuer + CodePath }

// codePage shows the authorization response it was sent to: the code and the
// state, as one `code#state` to paste, which lets the program check the state
// as it would on its own redirect. Nothing is decided here: the code is worth
// nothing without the program's PKCE verifier and, for a session, its DPoP
// key.
func (s *server) codePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("iss") != "" && q.Get("iss") != s.cfg.Issuer:
		writePage(w, http.StatusBadRequest, "This sign-in cannot be completed",
			`<p>The answer names another server than this one. Close this window and start again with <b>w17ctl login</b>.</p>`, false)
	case q.Get("error") == "access_denied":
		writePage(w, http.StatusOK, "Sign-in declined",
			`<p>Nothing was signed in. Close this window; <b>w17ctl login</b> starts again.</p>`, false)
	case q.Get("error") != "":
		writePage(w, http.StatusBadRequest, "This sign-in cannot be completed",
			`<p>`+html.EscapeString(q.Get("error_description"))+`</p><p class="error">Error: `+
				html.EscapeString(q.Get("error"))+`</p><p>Close this window and start again with <b>w17ctl login</b>.</p>`, false)
	case q.Get("code") == "" || q.Get("state") == "":
		writePage(w, http.StatusBadRequest, "Nothing to show",
			`<p>This page shows the code of a sign-in you approved. Start one with <b>w17ctl login</b>.</p>`, false)
	default:
		writePage(w, http.StatusOK, "Your sign-in code",
			`<p>Copy this code and paste it into <b>w17ctl</b>, where it asks for it:</p>`+
				`<div class="code" id="code">`+html.EscapeString(q.Get("code")+"#"+q.Get("state"))+`</div>`+
				`<div class="row"><button type="button" id="copy">Copy</button><span id="copied" role="status" aria-live="polite"></span></div>`+
				`<p class="muted">Paste it only into a w17ctl you started yourself just now: whoever has it can `+
				`finish that sign-in. It works once, and only for a few minutes.</p>`, true)
	}
}
