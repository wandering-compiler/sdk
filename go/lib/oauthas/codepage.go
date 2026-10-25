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
		codePageWrite(w, http.StatusBadRequest, "This sign-in cannot be completed",
			`<p>The answer names another server than this one. Close this window and start again with <b>w17ctl login</b>.</p>`)
	case q.Get("error") == "access_denied":
		codePageWrite(w, http.StatusOK, "Sign-in declined",
			`<p>Nothing was signed in. Close this window; <b>w17ctl login</b> starts again.</p>`)
	case q.Get("error") != "":
		codePageWrite(w, http.StatusBadRequest, "This sign-in cannot be completed",
			`<p>`+html.EscapeString(q.Get("error_description"))+`</p><p style="color:dimgray">Error: `+
				html.EscapeString(q.Get("error"))+`</p><p>Close this window and start again with <b>w17ctl login</b>.</p>`)
	case q.Get("code") == "" || q.Get("state") == "":
		codePageWrite(w, http.StatusBadRequest, "Nothing to show",
			`<p>This page shows the code of a sign-in you approved. Start one with <b>w17ctl login</b>.</p>`)
	default:
		codePageWrite(w, http.StatusOK, "Your sign-in code",
			`<p>Copy this code and paste it into <b>w17ctl</b>, where it asks for it:</p>`+
				`<p style="font-family:ui-monospace,monospace;font-size:1rem;word-break:break-all;user-select:all;`+
				`padding:.75rem;border:1px solid #ccc;border-radius:6px">`+
				html.EscapeString(q.Get("code")+"#"+q.Get("state"))+`</p>`+
				`<p style="color:dimgray">Paste it only into a w17ctl you started yourself just now: whoever has it can `+
				`finish that sign-in. It works once, and only for a few minutes.</p>`)
	}
}

func codePageWrite(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">` +
		`<title>` + html.EscapeString(title) + `</title>` +
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">` +
		`<h1 style="font-size:1.25rem">` + html.EscapeString(title) + `</h1>` + body + `</body>`))
}
