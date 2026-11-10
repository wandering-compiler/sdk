package restgw

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
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
		code        string // the envelope's canonical gRPC code
		detail      string // the captcha code a client branches on
		sentence    string
		logged      string
	}{
		{"off", "", nil, true, 0, "", "", "", ""},
		{"solved", "good", v, true, 0, "", "", "", ""},
		{"missing", "", v, false, http.StatusForbidden, "PERMISSION_DENIED", CodeCaptchaRequired, MsgidCaptchaRequired, "refused:"},
		{"rejected", "forged", v, false, http.StatusForbidden, "PERMISSION_DENIED", CodeCaptchaInvalid, MsgidCaptchaInvalid, "refused:"},
		{"provider down", "down", v, false, http.StatusServiceUnavailable, "UNAVAILABLE", CodeCaptchaUnavailable, MsgidCaptchaUnavailable, "error:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
			if c.token != "" {
				r.Header.Set(captcha.Header, c.token)
			}
			w := httptest.NewRecorder()
			var sink bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&sink)
			got := RequireCaptcha(c.v, w, r)
			log.SetOutput(prev)
			if got != c.pass {
				t.Fatalf("RequireCaptcha = %v, want %v", got, c.pass)
			}
			if c.pass {
				if w.Body.Len() != 0 {
					t.Fatalf("a passed request was answered: %s", w.Body)
				}
				return
			}
			var env struct {
				Error struct {
					Code    string
					Message string
					Details []FieldError
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if w.Code != c.status || env.Error.Code != c.code {
				t.Fatalf("answered %d %s, want %d %s", w.Code, w.Body, c.status, c.code)
			}
			if env.Error.Message != c.sentence || len(env.Error.Details) != 1 || env.Error.Details[0].Code != c.detail {
				t.Fatalf("envelope %s: want the catalog sentence %q and one detail coded %s", w.Body, c.sentence, c.detail)
			}
			if l := sink.String(); !strings.Contains(l, c.logged) || (c.logged == "error:" && strings.Contains(l, "refused:")) {
				t.Fatalf("reported %q, want a %q line", l, c.logged)
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

// The captcha sentences are harvested, so a project can translate them.
func TestVocabulary_CarriesTheCaptchaSentences(t *testing.T) {
	for _, m := range []string{MsgidCaptchaRequired, MsgidCaptchaInvalid, MsgidCaptchaUnavailable} {
		if !slices.Contains(Vocabulary(), m) {
			t.Errorf("Vocabulary() misses %q", m)
		}
	}
}

// The provider weighs the token with the client's address: the one
// ClientAddrMiddleware resolved behind the trusted proxies, never the
// proxy's own and never a client's X-Forwarded-For from an untrusted peer.
func TestRequireCaptcha_SendsTheResolvedClientAddress(t *testing.T) {
	seen := make(chan string, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen <- req["remoteip"]
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer provider.Close()
	v, err := captcha.New(captcha.Config{Secret: "s", VerifyURL: provider.URL})
	if err != nil {
		t.Fatal(err)
	}
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	h := ClientAddrMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		RequireCaptcha(v, w, r)
	}))
	for _, c := range []struct{ peer, xff, want string }{
		{"10.0.0.5:1", "203.0.113.7", "203.0.113.7"},
		{"198.51.100.9:1", "203.0.113.7", "198.51.100.9"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/auth/sign-up", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-For", c.xff)
		r.Header.Set(captcha.Header, "good")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got := <-seen; got != c.want {
			t.Errorf("peer %s, X-Forwarded-For %s: the provider saw remoteip %q, want %q", c.peer, c.xff, got, c.want)
		}
	}
}
