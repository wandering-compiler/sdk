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
