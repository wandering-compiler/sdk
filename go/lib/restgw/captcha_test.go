package restgw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/captcha"
)

func TestRequireCaptcha(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["response"] {
		case "good":
			_, _ = w.Write([]byte(`{"success":true}`))
		case "down":
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
		}
	}))
	defer provider.Close()
	v, err := captcha.New(captcha.Config{Secret: "s", VerifyURL: provider.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, token string
		v           *captcha.Verifier
		pass        bool
		status      int
		code        string
	}{
		{"off", "", nil, true, 0, ""},
		{"solved", "good", v, true, 0, ""},
		{"missing", "", v, false, http.StatusForbidden, CodeCaptchaRequired},
		{"rejected", "forged", v, false, http.StatusForbidden, CodeCaptchaInvalid},
		{"provider down", "down", v, false, http.StatusServiceUnavailable, CodeCaptchaUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
			if c.token != "" {
				r.Header.Set(captcha.Header, c.token)
			}
			w := httptest.NewRecorder()
			if got := RequireCaptcha(c.v, w, r); got != c.pass {
				t.Fatalf("RequireCaptcha = %v, want %v", got, c.pass)
			}
			if c.pass {
				if w.Body.Len() != 0 {
					t.Fatalf("a passed request was answered: %s", w.Body)
				}
				return
			}
			var env struct {
				Error struct{ Code string } `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if w.Code != c.status || env.Error.Code != c.code {
				t.Fatalf("answered %d %s, want %d %s", w.Code, w.Body, c.status, c.code)
			}
		})
	}
}

// A browser sends the captcha header only if the preflight allows it.
func TestCORSDefaultsAllowTheCaptchaHeader(t *testing.T) {
	if !slices.Contains(defaultCORSAllowedHeaders(), captcha.Header) {
		t.Fatalf("default CORS allowed headers %v lack %s", defaultCORSAllowedHeaders(), captcha.Header)
	}
}
