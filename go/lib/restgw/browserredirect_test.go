package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func redirectResult(t *testing.T, target, fallback, param, token string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/marb/callback?code=abc", nil)
	restgw.WriteBrowserRedirect(rec, req, target, fallback, param, token)
	return rec.Result()
}

// TestWriteBrowserRedirect_SendsTheBrowserOnWithTheTokenInTheFragment —
// the whole point: the browser leaves with a session, and the session is
// in the part of the URL that never reaches a server.
func TestWriteBrowserRedirect_SendsTheBrowserOnWithTheTokenInTheFragment(t *testing.T) {
	res := redirectResult(t, "/admin", "/", "token", "ey.JWT.value")
	if res.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if loc != "/admin#token=ey.JWT.value" {
		t.Errorf("Location = %q, want the target with the token in the fragment", loc)
	}
	// The credential is after the '#'. If it ever moves to the query it
	// reaches every proxy and access log between here and the page.
	if q := strings.SplitN(loc, "#", 2)[0]; strings.Contains(q, "ey.JWT.value") {
		t.Errorf("the token leaked into the part of the URL servers can see: %q", loc)
	}
}

// TestWriteBrowserRedirect_IsNotCacheable — a 302 carrying a credential
// must not be stored, or a shared cache serves one user's fragment to
// the next request for the same URL.
func TestWriteBrowserRedirect_IsNotCacheable(t *testing.T) {
	res := redirectResult(t, "/admin", "/", "token", "secret")
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on a redirect that carries a credential", cc)
	}
}

// TestWriteBrowserRedirect_EmptyTargetTakesTheFallback — the response
// field is allowed to be empty (the caller asked for no particular
// landing place); the fallback is what makes that a destination rather
// than a loop back onto the current URL.
func TestWriteBrowserRedirect_EmptyTargetTakesTheFallback(t *testing.T) {
	res := redirectResult(t, "", "/admin", "token", "x")
	if loc := res.Header.Get("Location"); loc != "/admin#token=x" {
		t.Errorf("Location = %q, want the fallback", loc)
	}
}

// TestWriteBrowserRedirect_NoTokenNoFragment — a redirect that carries
// nothing should not render a bare '#', which reads as "the token did
// not arrive".
func TestWriteBrowserRedirect_NoTokenNoFragment(t *testing.T) {
	res := redirectResult(t, "/done", "/", "token", "")
	if loc := res.Header.Get("Location"); loc != "/done" {
		t.Errorf("Location = %q, want no fragment at all", loc)
	}
}

// TestWriteBrowserRedirect_RefusesATargetThatLeavesTheSite — the guard
// that matters. Each of these is a way out that a `HasPrefix(p, "/")`
// check lets through, and the endpoint being guarded is the one that
// hands out credentials.
func TestWriteBrowserRedirect_RefusesATargetThatLeavesTheSite(t *testing.T) {
	for name, target := range map[string]string{
		"absolute":        "https://evil.example/steal",
		"scheme_relative": "//evil.example/steal",
		"backslash_form":  `/\evil.example/steal`,
		"header_split":    "/ok\r\nX-Injected: 1",
		"not_a_path":      "javascript:alert(1)",
		"control_char":    "/ok\x01",
	} {
		t.Run(name, func(t *testing.T) {
			res := redirectResult(t, target, "/", "token", "secret")
			if res.StatusCode == http.StatusFound {
				t.Fatalf("redirected to %q — an open redirect on the endpoint that mints sessions is a phishing flow", res.Header.Get("Location"))
			}
			if res.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502: the fault is the upstream handler's, not the caller's", res.StatusCode)
			}
			// And the credential must not travel in the refusal either.
			body := make([]byte, 512)
			n, _ := res.Body.Read(body)
			if strings.Contains(string(body[:n]), "secret") {
				t.Errorf("the refusal body carries the token: %s", body[:n])
			}
		})
	}
}

// TestIsSiteRelativeRedirect — the predicate on its own, so a future
// caller gets the same answer without re-deriving the rule.
func TestIsSiteRelativeRedirect(t *testing.T) {
	for _, ok := range []string{"/", "/admin", "/admin/users?tab=1", "/a/b/c"} {
		if !restgw.IsSiteRelativeRedirect(ok) {
			t.Errorf("%q should be accepted — it stays on the site", ok)
		}
	}
	for _, bad := range []string{
		"", "admin", "//host", `/\host`, "http://host", "https://host",
		"/x\nLocation: /y", "/x\tz",
	} {
		if restgw.IsSiteRelativeRedirect(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// --- the external variant: starting a federated sign-in --------------

func externalResult(t *testing.T, target, fallback string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/marb/authorize", nil)
	restgw.WriteBrowserRedirectExternal(rec, req, target, fallback)
	return rec.Result()
}

// TestWriteBrowserRedirectExternal_SendsTheBrowserToTheProvider — the
// start of the flow. The target is the identity provider's own URL, so
// it is absolute by nature.
func TestWriteBrowserRedirectExternal_SendsTheBrowserToTheProvider(t *testing.T) {
	const idp = "https://idp.example/authorize?client_id=abc&state=xyz"
	res := externalResult(t, idp, "/")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != idp {
		t.Errorf("Location = %q, want the provider's URL verbatim", loc)
	}
}

// TestWriteBrowserRedirectExternal_StillRefusesAScriptTarget — absolute
// does not mean anything. `javascript:` and `data:` run code in this
// page's place, which no provider needs and every XSS wants.
func TestWriteBrowserRedirectExternal_StillRefusesAScriptTarget(t *testing.T) {
	for name, target := range map[string]string{
		"javascript":   "javascript:alert(document.cookie)",
		"data":         "data:text/html,<script>alert(1)</script>",
		"file":         "file:///etc/passwd",
		"no_host":      "https://",
		"header_split": "https://idp.example/\r\nX-Injected: 1",
	} {
		t.Run(name, func(t *testing.T) {
			res := externalResult(t, target, "/")
			if res.StatusCode == http.StatusFound {
				t.Fatalf("redirected to %q", res.Header.Get("Location"))
			}
		})
	}
}

// TestWriteBrowserRedirect_SiteOnlyStillRefusesAbsolute — the two
// functions must not converge. The ordinary one is what the
// credential-bearing callback uses, and it stays shut.
func TestWriteBrowserRedirect_SiteOnlyStillRefusesAbsolute(t *testing.T) {
	res := redirectResult(t, "https://idp.example/x", "/", "token", "secret")
	if res.StatusCode == http.StatusFound {
		t.Fatalf("the credential-bearing redirect left the site: %q", res.Header.Get("Location"))
	}
}
