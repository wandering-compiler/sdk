package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stub is a siteverify that answers from a script and keeps what it was
// sent.
type stub struct {
	mu   sync.Mutex
	got  []map[string]string
	next func(n int, req map[string]string) (int, any)
}

func (s *stub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.got = append(s.got, req)
		n := len(s.got)
		s.mu.Unlock()
		status, body := s.next(n, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func answer(success bool, host string, codes ...string) map[string]any {
	return map[string]any{"success": success, "hostname": host, "error-codes": codes}
}

func verifier(t *testing.T, url string, hosts ...string) *Verifier {
	t.Helper()
	v, err := New(Config{Secret: "sec-1", VerifyURL: url, Hostnames: hosts})
	if err != nil || v == nil {
		t.Fatalf("New: %v, %v", v, err)
	}
	return v
}

func TestVerify_NoSecretIsOff(t *testing.T) {
	v, err := New(Config{})
	if err != nil || v != nil || v.Enabled() {
		t.Fatalf("New without a secret = %v, %v; want nil (off)", v, err)
	}
	if err := v.Verify(context.Background(), "", ""); err != nil {
		t.Fatalf("an off verifier refused: %v", err)
	}
}

func TestVerify_ASolvedTokenPassesAndTheProviderSeesTheCall(t *testing.T) {
	s := &stub{next: func(int, map[string]string) (int, any) { return 200, answer(true, "app.example") }}
	srv := s.serve(t)
	if err := verifier(t, srv.URL).Verify(context.Background(), " tok-1 ", "203.0.113.9"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := s.got[0]
	if got["secret"] != "sec-1" || got["response"] != "tok-1" || got["remoteip"] != "203.0.113.9" || len(got["idempotency_key"]) != 32 {
		t.Fatalf("siteverify got %v", got)
	}
}

func TestVerify_Refusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		token  string
		hosts  []string
		status int
		body   any
		want   error
		calls  int
	}{
		{"no token", "", nil, 200, answer(true, ""), ErrMissing, 0},
		{"too long", strings.Repeat("x", maxToken+1), nil, 200, answer(true, ""), ErrRejected, 0},
		{"used or expired", "t", nil, 200, answer(false, "", "timeout-or-duplicate"), ErrRejected, 1},
		{"forged", "t", nil, 200, answer(false, "", "invalid-input-response"), ErrRejected, 1},
		{"another site", "t", []string{"app.example"}, 200, answer(true, "evil.example"), ErrRejected, 1},
		{"our secret refused", "t", nil, 200, answer(false, "", "invalid-input-secret"), ErrUnavailable, 1},
		{"provider down", "t", nil, 502, map[string]any{}, ErrUnavailable, 2},
		{"provider internal error", "t", nil, 200, answer(false, "", "internal-error"), ErrUnavailable, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &stub{next: func(int, map[string]string) (int, any) { return c.status, c.body }}
			srv := s.serve(t)
			err := verifier(t, srv.URL, c.hosts...).Verify(context.Background(), c.token, "")
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
			if len(s.got) != c.calls {
				t.Fatalf("siteverify called %d times, want %d", len(s.got), c.calls)
			}
		})
	}
}

// A retry after a provider error re-sends the same idempotency key, so the
// provider answers it as the first check instead of "already used".
func TestVerify_ARetryKeepsItsIdempotencyKey(t *testing.T) {
	s := &stub{next: func(n int, _ map[string]string) (int, any) {
		if n == 1 {
			return 200, answer(false, "", "internal-error")
		}
		return 200, answer(true, "app.example")
	}}
	srv := s.serve(t)
	if err := verifier(t, srv.URL, "APP.example").Verify(context.Background(), "t", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(s.got) != 2 || s.got[0]["idempotency_key"] != s.got[1]["idempotency_key"] {
		t.Fatalf("calls %v", s.got)
	}
}

func TestNew_RefusesAnUnknownProvider(t *testing.T) {
	if _, err := New(Config{Secret: "s", Provider: "turnstyle"}); err == nil || !strings.Contains(err.Error(), "turnstyle") {
		t.Fatalf("New with a misspelt provider = %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("APP_CAPTCHA_SECRET", " s ")
	t.Setenv("APP_CAPTCHA_PROVIDER", "Turnstile")
	t.Setenv("APP_CAPTCHA_VERIFY_URL", "http://stub/verify")
	t.Setenv("APP_CAPTCHA_TIMEOUT", "2s")
	t.Setenv("APP_CAPTCHA_HOSTNAMES", "a.example, B.example,,")
	cfg, err := FromEnv("APP")
	if err != nil || cfg.Secret != "s" || cfg.Provider != ProviderTurnstile || cfg.VerifyURL != "http://stub/verify" ||
		cfg.Timeout.String() != "2s" || strings.Join(cfg.Hostnames, ",") != "a.example,b.example" {
		t.Fatalf("FromEnv = %+v, %v", cfg, err)
	}
	t.Setenv("APP_CAPTCHA_TIMEOUT", "soon")
	if _, err := FromEnv("APP"); err == nil {
		t.Fatal("a timeout that is not a duration was accepted")
	}
}
