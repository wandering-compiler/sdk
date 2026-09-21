package restgw

import (
	"net/http"
	"net/url"
	"strings"
)

// WriteBrowserRedirect answers a request the BROWSER is walking with a
// 302, instead of a JSON body it has no way to act on.
//
// The case it exists for is an OAuth / OIDC callback: the identity
// provider sends the browser to the gateway with `?code=…`, the handler
// exchanges it for a session, and the browser has to end up back in the
// application holding that session. Answering with `{"token":"ey…"}`
// leaves the user looking at a credential in a document.
//
// # The token rides the FRAGMENT
//
// `Location: <target>#<param>=<token>`. A fragment is never sent to a
// server — not to the redirect target, not through a proxy, not into an
// access log, not in a `Referer` — so the credential reaches the page
// and nothing in between. The same value in the query string is visible
// to every hop, and that is how OAuth deployments leak sessions. The
// page reads it from `location.hash` and strips it.
//
// # target is checked here, and it is not the handler's check repeated
//
// The handler validates what the CALLER asked for before signing it into
// the flow's state. This validates what the HANDLER RETURNED — a
// different value, produced later, by code that may have been changed
// since. An open redirect on the endpoint that hands out credentials is
// the shape a phishing flow needs, so it gets two independent guards
// rather than one guard trusted twice.
//
// An unsafe target is answered 502: the gateway will not act on it, and
// the fault is upstream of the caller, who did nothing wrong.
func WriteBrowserRedirect(w http.ResponseWriter, r *http.Request, target, fallback, tokenParam, token string) {
	writeBrowserRedirect(w, r, target, fallback, tokenParam, token, false)
}

// WriteBrowserRedirectExternal is [WriteBrowserRedirect] for an endpoint
// that legitimately sends the browser OFF this site — the start of a
// federated sign-in, where the destination is the identity provider's
// own authorize URL.
//
// It is a separate function rather than a flag so the call site says
// which one it is: a reader of generated code sees "this one leaves the
// site" without resolving a boolean. The parser refuses to pair it with
// a credential, so nothing that calls this is handing out a session.
func WriteBrowserRedirectExternal(w http.ResponseWriter, r *http.Request, target, fallback string) {
	writeBrowserRedirect(w, r, target, fallback, "", "", true)
}

func writeBrowserRedirect(w http.ResponseWriter, r *http.Request, target, fallback, tokenParam, token string, external bool) {
	dest := strings.TrimSpace(target)
	if dest == "" {
		dest = fallback
	}
	if !redirectTargetOK(dest, external) {
		WriteError(w, http.StatusBadGateway, "INTERNAL",
			"the upstream handler returned a redirect target this gateway will not send a browser to")
		return
	}
	if token != "" {
		param := tokenParam
		if param == "" {
			param = "token"
		}
		dest += "#" + url.QueryEscape(param) + "=" + url.QueryEscape(token)
	}
	// A redirect carrying a credential must not be stored by anything.
	// Without this a shared cache can serve one user's fragment to the
	// next request for the same URL.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, dest, http.StatusFound)
}

// redirectTargetOK accepts a destination for this redirect.
//
// Site-relative is always fine. An `external` endpoint additionally
// accepts an absolute http(s) URL — and nothing else: a `javascript:`
// or `data:` target is a script the browser runs in this page's place,
// which no OAuth provider ever needs and every XSS wants. Control
// characters are refused on both paths; they split the header.
func redirectTargetOK(dest string, external bool) bool {
	if IsSiteRelativeRedirect(dest) {
		return true
	}
	if !external {
		return false
	}
	for _, r := range dest {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(dest)
	if err != nil {
		return false
	}
	return (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// IsSiteRelativeRedirect reports whether a redirect target stays on this
// site.
//
// The rule is narrower than "starts with a slash", and each exclusion is
// a real way out:
//
//   - `//evil.example` is SCHEME-RELATIVE. A browser reads it as a host,
//     so it leaves the site while looking like a path. This is the form
//     a `strings.HasPrefix(p, "/")` check lets through, and it is the
//     single most common open-redirect bug.
//   - `/\evil.example` is the same trick with a backslash, which several
//     browsers normalise to `//`.
//   - anything carrying `://` is absolute however it got that way.
//   - a control character or a newline can split the header, so a target
//     carrying one is refused rather than sanitised — the value came
//     from a handler that should not be producing them.
func IsSiteRelativeRedirect(p string) bool {
	if !strings.HasPrefix(p, "/") ||
		strings.HasPrefix(p, "//") ||
		strings.HasPrefix(p, `/\`) ||
		strings.Contains(p, "://") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
