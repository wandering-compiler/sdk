package oauthas

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The code page shows the answer it was sent, as one `code#state` to paste,
// escaped, and nothing it was not sent.
func TestCodePage(t *testing.T) {
	h := mount(t, Config{}, &fakeBackend{})
	for name, c := range map[string]struct {
		query   string
		status  int
		want    []string
		mustNot []string
	}{
		"code": {
			query:  "code=abc&state=xyz&iss=https%3A%2F%2Fauth.example.com",
			status: http.StatusOK,
			want:   []string{"abc#xyz", "Your sign-in code"},
		},
		"code without iss": {
			query:  "code=abc&state=xyz",
			status: http.StatusOK,
			want:   []string{"abc#xyz"},
		},
		"escaped": {
			query:   "code=%3Cscript%3E&state=s",
			status:  http.StatusOK,
			want:    []string{"&lt;script&gt;#s"},
			mustNot: []string{"<script>"},
		},
		"another issuer": {
			query:   "code=abc&state=xyz&iss=https%3A%2F%2Fevil.example",
			status:  http.StatusBadRequest,
			want:    []string{"another server"},
			mustNot: []string{"abc#xyz"},
		},
		"denied": {
			query:  "error=access_denied&state=xyz",
			status: http.StatusOK,
			want:   []string{"Sign-in declined"},
		},
		"refused": {
			query:  "error=invalid_request&error_description=%3Cb%3Ebad%3C%2Fb%3E&state=xyz",
			status: http.StatusBadRequest,
			want:   []string{"&lt;b&gt;bad&lt;/b&gt;", "invalid_request"},
		},
		"no state": {
			query:   "code=abc",
			status:  http.StatusBadRequest,
			want:    []string{"Nothing to show"},
			mustNot: []string{"abc"},
		},
	} {
		w := do(h, httptest.NewRequest(http.MethodGet, CodePath+"?"+c.query, nil))
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", name, w.Code, c.status)
		}
		body := w.Body.String()
		for _, s := range c.want {
			if !strings.Contains(body, s) {
				t.Errorf("%s: body lacks %q: %s", name, s, body)
			}
		}
		for _, s := range c.mustNot {
			if strings.Contains(body, s) {
				t.Errorf("%s: body has %q: %s", name, s, body)
			}
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", name, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy %q", name, got)
		}
	}
	if w := do(h, httptest.NewRequest(http.MethodPost, CodePath+"?code=a&state=b", nil)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d", w.Code)
	}
	if got := CodeRedirectURI("https://auth.example.com"); got != "https://auth.example.com/oauth/code" {
		t.Errorf("CodeRedirectURI = %q", got)
	}
}

// pageNonces returns the nonce the CSP header allows for styles and for
// scripts ("" when it allows none), and fails when the header is not the
// strict one every page shown to a person carries.
func pageNonces(t *testing.T, name string, w *httptest.ResponseRecorder) (style, script string) {
	t.Helper()
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("%s: CSP %q lacks %q", name, csp, want)
		}
	}
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		for prefix, dst := range map[string]*string{"style-src 'nonce-": &style, "script-src 'nonce-": &script} {
			if rest, ok := strings.CutPrefix(d, prefix); ok {
				*dst = strings.TrimSuffix(rest, "'")
			}
		}
	}
	if style == "" {
		t.Errorf("%s: CSP %q allows no style", name, csp)
	}
	for h, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if got := w.Header().Get(h); got != want {
			t.Errorf("%s: %s %q, want %q", name, h, got, want)
		}
	}
	return style, script
}

// The pages shown to a person style themselves through a per-response nonce:
// one <style> and, on the code page only, one <script>, both carrying the
// nonce the CSP header allows, and no inline style attribute the CSP would
// block.
func TestPagesCarryTheirOwnNonce(t *testing.T) {
	h := mount(t, Config{}, &fakeBackend{})
	seen := map[string]bool{}
	for name, c := range map[string]struct {
		path       string
		wantScript bool
	}{
		"code":          {CodePath + "?code=abc&state=xyz", true},
		"code again":    {CodePath + "?code=abc&state=xyz", true},
		"code refused":  {CodePath + "?error=invalid_request&state=xyz", false},
		"authorize err": {AuthorizePath + "?client_id=a&client_id=b", false},
	} {
		w := do(h, httptest.NewRequest(http.MethodGet, c.path, nil))
		body := w.Body.String()
		style, script := pageNonces(t, name, w)
		if seen[style] {
			t.Errorf("%s: nonce %q reused across responses", name, style)
		}
		seen[style] = true
		if n := strings.Count(body, "<style"); n != 1 || !strings.Contains(body, `<style nonce="`+style+`">`) {
			t.Errorf("%s: want one <style> with the CSP nonce, %d in %s", name, n, body)
		}
		if strings.Contains(body, "style=") {
			t.Errorf("%s: an inline style attribute the CSP blocks: %s", name, body)
		}
		if c.wantScript {
			if script != style || strings.Count(body, "<script") != 1 || !strings.Contains(body, `<script nonce="`+script+`">`) {
				t.Errorf("%s: want one <script> with the CSP nonce %q: %s", name, script, body)
			}
			if strings.Count(body, "abc#xyz") != 1 {
				t.Errorf("%s: the code appears %d times, want once", name, strings.Count(body, "abc#xyz"))
			}
		} else if script != "" || strings.Contains(body, "<script") {
			t.Errorf("%s: a page without the copy button allows or carries a script: %q", name, script)
		}
		if name == "authorize err" && (w.Code != http.StatusBadRequest || !strings.Contains(body, "client_id is repeated")) {
			t.Errorf("%s: status %d body %s", name, w.Code, body)
		}
	}
}
