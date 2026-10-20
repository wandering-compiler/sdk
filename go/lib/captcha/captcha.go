// Package captcha verifies the solved captcha a browser sends with a call
// the gateway guards (RestEndpoint.captcha): the token the provider's widget
// gave the page, checked server-side with the provider before the backend is
// called. Cloudflare Turnstile is the provider; the verification is the
// siteverify shape hCaptcha and reCAPTCHA share, so another one is a URL and
// a name, not a new path.
//
// Off unless configured: a gateway with no secret verifies nothing (New
// returns nil and Verify on nil passes), so a project turns captchas on by
// setting `<PREFIX>_CAPTCHA_SECRET` and off by removing it.
package captcha

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Header is the request header a call carries its captcha token in.
const Header = "W17-Captcha-Token"

// ProviderTurnstile is Cloudflare Turnstile, the default provider.
const ProviderTurnstile = "turnstile"

// TurnstileVerifyURL is Turnstile's siteverify endpoint.
const TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// maxToken is the longest token Turnstile issues; a longer header is refused
// without asking it.
const maxToken = 2048

// The outcomes Verify refuses with. A caller tells them apart with
// errors.Is: the first two are the caller's (403), the last is the
// provider's or the configuration's (503 — the gateway fails closed).
var (
	// ErrMissing: the call carried no token.
	ErrMissing = errors.New("captcha: no token")
	// ErrRejected: the provider answered that the token is not a solved,
	// unused captcha for this site (expired, already used, forged, or
	// solved for another hostname).
	ErrRejected = errors.New("captcha: token rejected")
	// ErrUnavailable: the provider could not be asked, or refused the
	// gateway's own secret.
	ErrUnavailable = errors.New("captcha: verification unavailable")
)

// Config is a Verifier's settings; FromEnv reads them.
type Config struct {
	// Secret is the provider's secret key. Empty turns verification off.
	Secret string
	// Provider names the provider; only ProviderTurnstile ("" means it).
	Provider string
	// VerifyURL overrides the provider's siteverify URL (a stub in tests).
	VerifyURL string
	// Timeout bounds one siteverify call; 0 means 5 seconds.
	Timeout time.Duration
	// Hostnames, when set, is the allowlist the answer's hostname (the site
	// the widget was solved on) must be in.
	Hostnames []string
	// Client is the HTTP client; nil means one with Timeout.
	Client *http.Client
}

// FromEnv reads `<prefix>_CAPTCHA_SECRET`, `_PROVIDER`, `_VERIFY_URL`,
// `_TIMEOUT` (a Go duration) and `_HOSTNAMES` (comma-separated).
func FromEnv(prefix string) (Config, error) {
	name := func(s string) string { return prefix + "_CAPTCHA_" + s }
	cfg := Config{
		Secret:    strings.TrimSpace(os.Getenv(name("SECRET"))),
		Provider:  strings.ToLower(strings.TrimSpace(os.Getenv(name("PROVIDER")))),
		VerifyURL: strings.TrimSpace(os.Getenv(name("VERIFY_URL"))),
	}
	if v := strings.TrimSpace(os.Getenv(name("TIMEOUT"))); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s=%q is not a positive duration (e.g. 5s)", name("TIMEOUT"), v)
		}
		cfg.Timeout = d
	}
	for _, h := range strings.Split(os.Getenv(name("HOSTNAMES")), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			cfg.Hostnames = append(cfg.Hostnames, h)
		}
	}
	return cfg, nil
}

// Verifier checks tokens with the provider. A nil *Verifier is off: Verify
// passes every call.
type Verifier struct {
	secret    string
	url       string
	client    *http.Client
	hostnames map[string]bool
}

// New builds the Verifier cfg describes, or nil (off) when cfg has no
// secret. An unknown provider is refused: a typo must not leave a surface
// silently unguarded or guarded by the wrong service.
func New(cfg Config) (*Verifier, error) {
	if cfg.Secret == "" {
		return nil, nil
	}
	switch cfg.Provider {
	case "", ProviderTurnstile:
	default:
		return nil, fmt.Errorf("captcha: unknown provider %q (supported: %s)", cfg.Provider, ProviderTurnstile)
	}
	v := &Verifier{secret: cfg.Secret, url: cfg.VerifyURL, client: cfg.Client}
	if v.url == "" {
		v.url = TurnstileVerifyURL
	}
	if v.client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		v.client = &http.Client{Timeout: timeout}
	}
	if len(cfg.Hostnames) > 0 {
		v.hostnames = map[string]bool{}
		for _, h := range cfg.Hostnames {
			v.hostnames[strings.ToLower(h)] = true
		}
	}
	return v, nil
}

// NewFromEnv is FromEnv and New, logging once which way it went: a guarded
// endpoint served unguarded must not be silent.
func NewFromEnv(prefix string) (*Verifier, error) {
	cfg, err := FromEnv(prefix)
	if err != nil {
		return nil, err
	}
	v, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if v == nil {
		log.Printf("captcha: OFF (%s_CAPTCHA_SECRET is not set) — endpoints that require a captcha are served without one", prefix)
	} else {
		log.Printf("captcha: ON (%s, %s)", providerName(cfg.Provider), v.url)
	}
	return v, nil
}

func providerName(p string) string {
	if p == "" {
		return ProviderTurnstile
	}
	return p
}

// Enabled reports whether v verifies anything.
func (v *Verifier) Enabled() bool { return v != nil }

// siteverify is the provider's answer.
type siteverify struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
	Hostname   string   `json:"hostname"`
}

// Verify checks token, solved by a client at remoteIP ("" when unknown),
// with the provider. nil means the call may proceed; otherwise the error
// wraps ErrMissing, ErrRejected or ErrUnavailable. A token is single use:
// the provider answers a second check of it as rejected.
func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) error {
	if v == nil {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissing
	}
	if len(token) > maxToken {
		return fmt.Errorf("%w: longer than %d bytes", ErrRejected, maxToken)
	}
	// One idempotency key for the call and its retry: the provider answers
	// the retry of a token it already checked with the first answer
	// instead of "already used".
	var key [16]byte
	_, _ = rand.Read(key[:])
	body, _ := json.Marshal(map[string]string{
		"secret":          v.secret,
		"response":        token,
		"remoteip":        remoteIP,
		"idempotency_key": hex.EncodeToString(key[:]),
	})
	var ans siteverify
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ans, err = v.ask(ctx, body)
		if err == nil && !contains(ans.ErrorCodes, "internal-error") {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if contains(ans.ErrorCodes, "invalid-input-secret") || contains(ans.ErrorCodes, "missing-input-secret") {
		// The gateway's configuration, not the caller: say so where an
		// operator looks, and fail closed.
		log.Printf("captcha: the provider refused the configured secret (%s)", strings.Join(ans.ErrorCodes, ", "))
		return fmt.Errorf("%w: the provider refused the gateway's secret", ErrUnavailable)
	}
	if contains(ans.ErrorCodes, "internal-error") {
		return fmt.Errorf("%w: the provider answered internal-error", ErrUnavailable)
	}
	if !ans.Success {
		return fmt.Errorf("%w: %s", ErrRejected, strings.Join(ans.ErrorCodes, ", "))
	}
	if v.hostnames != nil && !v.hostnames[strings.ToLower(ans.Hostname)] {
		return fmt.Errorf("%w: solved on %q, which is not an allowed hostname", ErrRejected, ans.Hostname)
	}
	return nil
}

func (v *Verifier) ask(ctx context.Context, body []byte) (siteverify, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url, bytes.NewReader(body))
	if err != nil {
		return siteverify{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return siteverify{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return siteverify{}, err
	}
	if resp.StatusCode >= 500 {
		return siteverify{}, fmt.Errorf("siteverify answered HTTP %d", resp.StatusCode)
	}
	var ans siteverify
	if err := json.Unmarshal(raw, &ans); err != nil {
		return siteverify{}, fmt.Errorf("siteverify answered HTTP %d without JSON", resp.StatusCode)
	}
	return ans, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
