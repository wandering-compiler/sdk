package dpop_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/cryptosigner"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

const testOrigin = "https://api.example.com"

var nonceKey = []byte("0123456789abcdef0123456789abcdef")

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_800_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newVerifier(t *testing.T, nonces dpop.NonceSource, mods ...func(*dpop.Config)) *dpop.Verifier {
	t.Helper()
	cfg := dpop.Config{Origin: testOrigin, Nonces: nonces}
	for _, m := range mods {
		m(&cfg)
	}
	v, err := dpop.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func hmacNonces(t *testing.T, c *clock, bucket time.Duration) *dpop.HMACNonces {
	t.Helper()
	n, err := dpop.NewHMACNonces(nonceKey, dpop.WithNonceClock(c.Now), dpop.WithNonceBucket(bucket))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustKey(t *testing.T) *dpop.Key {
	t.Helper()
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, k dpop.Signer, c dpop.Claims) string {
	t.Helper()
	p, err := dpop.SignClaims(k, c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// claims returns valid claims for GET /r at the clock's time.
func claims(c *clock, nonce string) dpop.Claims {
	return dpop.Claims{JTI: "jti-" + nonce + c.Now().String(), HTM: "GET", HTU: testOrigin + "/r", IAT: c.Now().Unix(), Nonce: nonce}
}

func get(proof string) dpop.Request {
	return dpop.Request{Method: "GET", Path: "/r", Proofs: []string{proof}}
}

// signRaw signs an arbitrary payload as a DPoP proof with go-jose.
func signRaw(t *testing.T, k dpop.Signer, payload string) string {
	t.Helper()
	opts := (&jose.SignerOptions{EmbedJWK: true}).WithType(dpop.ProofType)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: cryptosigner.Opaque(k)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := s.Sign([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rewrite replaces the header and/or payload segment of a compact JWS,
// keeping the signature (which then no longer verifies).
func rewrite(t *testing.T, proof string, hdr, payload func(map[string]any)) string {
	t.Helper()
	parts := strings.Split(proof, ".")
	edit := func(seg string, f func(map[string]any)) string {
		if f == nil {
			return seg
		}
		m := map[string]any{}
		if err := json.Unmarshal(b64(t, seg), &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		raw, _ := json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return edit(parts[0], hdr) + "." + edit(parts[1], payload) + "." + parts[2]
}

// The proofs printed in RFC 9449 (Figure 2, a token request, and Figure 13,
// a resource request with ath) verify, with nonces off and the clock at
// their iat.
func TestVerify_RFC9449Examples(t *testing.T) {
	fig2 := "eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7Imt0eSI6Ik" +
		"VDIiwieCI6Imw4dEZyaHgtMzR0VjNoUklDUkRZOXpDa0RscEJoRjQyVVFVZldWQVdCR" +
		"nMiLCJ5IjoiOVZFNGpmX09rX282NHpiVFRsY3VOSmFqSG10NnY5VERWclUwQ2R2R1JE" +
		"QSIsImNydiI6IlAtMjU2In19.eyJqdGkiOiItQndDM0VTYzZhY2MybFRjIiwiaHRtIj" +
		"oiUE9TVCIsImh0dSI6Imh0dHBzOi8vc2VydmVyLmV4YW1wbGUuY29tL3Rva2VuIiwia" +
		"WF0IjoxNTYyMjYyNjE2fQ.2-GxA6T8lP4vfrg8v-FdWP0A0zdrj8igiMLvqRMUvwnQg" +
		"4PtFLbdLXiOSsX0x7NVY-FNyJK70nfbV37xRZT3Lg"
	fig13 := "eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7Imt0eSI6Ik" +
		"VDIiwieCI6Imw4dEZyaHgtMzR0VjNoUklDUkRZOXpDa0RscEJoRjQyVVFVZldWQVdCR" +
		"nMiLCJ5IjoiOVZFNGpmX09rX282NHpiVFRsY3VOSmFqSG10NnY5VERWclUwQ2R2R1JE" +
		"QSIsImNydiI6IlAtMjU2In19.eyJqdGkiOiJlMWozVl9iS2ljOC1MQUVCIiwiaHRtIj" +
		"oiR0VUIiwiaHR1IjoiaHR0cHM6Ly9yZXNvdXJjZS5leGFtcGxlLm9yZy9wcm90ZWN0Z" +
		"WRyZXNvdXJjZSIsImlhdCI6MTU2MjI2MjYxOCwiYXRoIjoiZlVIeU8ycjJaM0RaNTNF" +
		"c05yV0JiMHhXWG9hTnk1OUlpS0NBcWtzbVFFbyJ9.2oW9RP35yRqzhrtNP86L-Ey71E" +
		"OptxRimPPToA1plemAgR6pxHF8y6-yqyVnmcw6Fy1dqd-jfxSYoMxhAJpLjA"
	const jkt = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"
	cases := []struct {
		name, origin string
		iat          int64
		req          dpop.Request
	}{
		{"figure 2", "https://server.example.com", 1562262616,
			dpop.Request{Method: "POST", Path: "/token", Proofs: []string{fig2}}},
		{"figure 13", "https://resource.example.org", 1562262618,
			dpop.Request{Method: "GET", Path: "/protectedresource", Proofs: []string{fig13},
				AccessToken: "Kz~8mXK1EalYznwH-LC-1fBAo.4Ljp~zsPE_NeO.gxU", Binding: jkt}},
	}
	for _, tc := range cases {
		v, err := dpop.NewVerifier(dpop.Config{Origin: tc.origin, Now: func() time.Time { return time.Unix(tc.iat, 0) }})
		if err != nil {
			t.Fatal(err)
		}
		tp, err := v.Verify(context.Background(), tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tp != jkt {
			t.Fatalf("%s: thumbprint %s, want %s", tc.name, tp, jkt)
		}
	}
}

// Every refusal cause comes back as its own error.
func TestVerify_RefusalCauses(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Minute)
	k := mustKey(t)
	tp := k.Thumbprint()
	nonce, _ := nonces.Issue(tp)
	seq := 0
	good := func(mod func(*dpop.Claims)) string {
		cl := claims(c, nonce)
		seq++
		cl.JTI = fmt.Sprintf("jti-%d", seq)
		if mod != nil {
			mod(&cl)
		}
		return sign(t, k, cl)
	}
	reqWith := func(proof string, mod func(*dpop.Request)) dpop.Request {
		r := get(proof)
		if mod != nil {
			mod(&r)
		}
		return r
	}
	ed, _ := dpop.GenerateKey(dpop.KeyEd25519)
	cases := []struct {
		name string
		req  func() dpop.Request
		want error
	}{
		{"no header", func() dpop.Request { return dpop.Request{Method: "GET", Path: "/r"} }, dpop.ErrMissingProof},
		{"empty header", func() dpop.Request { return reqWith("", nil) }, dpop.ErrMissingProof},
		{"two headers", func() dpop.Request {
			return reqWith(good(nil), func(r *dpop.Request) { r.Proofs = append(r.Proofs, good(nil)) })
		}, dpop.ErrMalformed},
		{"oversized", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["pad"] = strings.Repeat("a", dpop.MaxProofBytes) }, nil), nil)
		}, dpop.ErrMalformed},
		{"not a JWS", func() dpop.Request { return reqWith("garbage", nil) }, dpop.ErrMalformed},
		{"alg none", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["alg"] = "none" }, nil), nil)
		}, dpop.ErrBadAlg},
		{"alg HS256", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["alg"] = "HS256" }, nil), nil)
		}, dpop.ErrBadAlg},
		{"alg RS256", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["alg"] = "RS256" }, nil), nil)
		}, dpop.ErrBadAlg},
		{"alg ES384", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["alg"] = "ES384" }, nil), nil)
		}, dpop.ErrBadAlg},
		{"typ JWT", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["typ"] = "JWT" }, nil), nil)
		}, dpop.ErrBadType},
		{"typ missing", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { delete(h, "typ") }, nil), nil)
		}, dpop.ErrBadType},
		{"jwk missing", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { delete(h, "jwk") }, nil), nil)
		}, dpop.ErrBadKey},
		{"jwk with private d", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) {
				h["jwk"].(map[string]any)["d"] = "AAAA"
			}, nil), nil)
		}, dpop.ErrBadKey},
		{"jwk does not fit alg", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), func(h map[string]any) { h["alg"] = "EdDSA" }, nil), nil)
		}, dpop.ErrBadKey},
		{"Ed25519 jwk under ES256", func() dpop.Request {
			p := sign(t, ed, claims(c, nonce))
			return reqWith(rewrite(t, p, func(h map[string]any) { h["alg"] = "ES256" }, nil), nil)
		}, dpop.ErrBadKey},
		{"signature over other claims", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), nil, func(p map[string]any) { p["htu"] = testOrigin + "/other" }), nil)
		}, dpop.ErrBadSignature},
		{"no jti", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.JTI = "" }), nil) }, dpop.ErrMalformed},
		{"jti too long", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.JTI = strings.Repeat("j", dpop.MaxJTIBytes+1) }), nil)
		}, dpop.ErrMalformed},
		{"no htm", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.HTM = "" }), nil) }, dpop.ErrMalformed},
		{"tampered claims", func() dpop.Request {
			return reqWith(rewrite(t, good(nil), nil, func(m map[string]any) { delete(m, "iat") }), nil)
		}, dpop.ErrBadSignature},
		{"no iat", func() dpop.Request {
			return reqWith(signRaw(t, k, `{"jti":"x1","htm":"GET","htu":"`+testOrigin+`/r","nonce":"`+nonce+`"}`), nil)
		}, dpop.ErrMalformed},
		{"iat not a number", func() dpop.Request {
			return reqWith(signRaw(t, k, `{"jti":"x2","htm":"GET","htu":"`+testOrigin+`/r","iat":"now","nonce":"`+nonce+`"}`), nil)
		}, dpop.ErrMalformed},
		{"no htu", func() dpop.Request {
			return reqWith(signRaw(t, k, `{"jti":"x3","htm":"GET","iat":1,"nonce":"`+nonce+`"}`), nil)
		}, dpop.ErrMalformed},
		{"payload not JSON", func() dpop.Request { return reqWith(signRaw(t, k, `not json`), nil) }, dpop.ErrMalformed},
		{"missing nonce", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.Nonce = "" }), nil) }, dpop.ErrUseNonce},
		{"forged nonce", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.Nonce = "AAAA" }), nil) }, dpop.ErrUseNonce},
		{"nonce of another key", func() dpop.Request {
			other, _ := nonces.Issue(ed.Thumbprint())
			return reqWith(good(func(c *dpop.Claims) { c.Nonce = other }), nil)
		}, dpop.ErrUseNonce},
		{"htm mismatch", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.HTM = "POST" }), nil) }, dpop.ErrHTMMismatch},
		{"htm case", func() dpop.Request { return reqWith(good(func(c *dpop.Claims) { c.HTM = "get" }), nil) }, dpop.ErrHTMMismatch},
		{"htu other path", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.HTU = testOrigin + "/s" }), nil)
		}, dpop.ErrHTUMismatch},
		{"htu other host", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.HTU = "https://evil.example.com/r" }), nil)
		}, dpop.ErrHTUMismatch},
		{"htu other scheme", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.HTU = "http://api.example.com/r" }), nil)
		}, dpop.ErrHTUMismatch},
		{"ath for another token", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.ATH = dpop.AccessTokenHash("other") }), func(r *dpop.Request) {
				r.AccessToken, r.Binding = "tok", tp
			})
		}, dpop.ErrATHMismatch},
		{"ath missing with a token", func() dpop.Request {
			return reqWith(good(nil), func(r *dpop.Request) { r.AccessToken, r.Binding = "tok", tp })
		}, dpop.ErrATHMismatch},
		{"ath without a token", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.ATH = dpop.AccessTokenHash("tok") }), nil)
		}, dpop.ErrATHMismatch},
		{"binding to another key", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.ATH = dpop.AccessTokenHash("tok") }), func(r *dpop.Request) {
				r.AccessToken, r.Binding = "tok", ed.Thumbprint()
			})
		}, dpop.ErrBindingMismatch},
		{"token with no binding", func() dpop.Request {
			return reqWith(good(func(c *dpop.Claims) { c.ATH = dpop.AccessTokenHash("tok") }), func(r *dpop.Request) {
				r.AccessToken = "tok"
			})
		}, dpop.ErrBindingMismatch},
	}
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	all := []error{dpop.ErrMissingProof, dpop.ErrMalformed, dpop.ErrBadAlg, dpop.ErrBadType, dpop.ErrBadKey,
		dpop.ErrBadSignature, dpop.ErrUseNonce, dpop.ErrStaleProof, dpop.ErrReplay, dpop.ErrHTMMismatch,
		dpop.ErrHTUMismatch, dpop.ErrATHMismatch, dpop.ErrBindingMismatch}
	for _, tc := range cases {
		tp, err := v.Verify(context.Background(), tc.req())
		if !errors.Is(err, tc.want) || tp != "" {
			t.Errorf("%s: Verify = %q, %v; want %v", tc.name, tp, err, tc.want)
			continue
		}
		for _, other := range all {
			if other != tc.want && errors.Is(err, other) {
				t.Errorf("%s: %v also matches %v", tc.name, err, other)
			}
		}
	}

	// The control: the same shapes, valid, pass.
	if _, err := v.Verify(context.Background(), reqWith(good(func(c *dpop.Claims) { c.ATH = dpop.AccessTokenHash("tok") }),
		func(r *dpop.Request) { r.AccessToken, r.Binding = "tok", tp })); err != nil {
		t.Fatalf("valid proof refused: %v", err)
	}
}

// A refused nonce comes with a fresh one for the same key, which then works.
func TestVerify_NonceChallengeCarriesFreshNonce(t *testing.T) {
	c := newClock()
	v := newVerifier(t, hmacNonces(t, c, time.Minute), func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	_, err := v.Verify(context.Background(), get(sign(t, k, claims(c, ""))))
	var ne *dpop.NonceError
	if !errors.As(err, &ne) || !ne.Missing || ne.Nonce == "" {
		t.Fatalf("Verify without nonce = %v, want a NonceError with a nonce", err)
	}
	if _, err := v.Verify(context.Background(), get(sign(t, k, claims(c, ne.Nonce)))); err != nil {
		t.Fatalf("Verify with the issued nonce: %v", err)
	}
}

// Checks run in the documented order: signature before nonce, nonce before
// jti, jti before htm/htu, htu before ath, ath before binding.
func TestVerify_CheckOrder(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Minute)
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	other := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	ctx := context.Background()

	// Bad signature and no nonce → the signature is reported.
	p := rewrite(t, sign(t, k, claims(c, "")), nil, func(m map[string]any) { m["htm"] = "PUT" })
	if _, err := v.Verify(ctx, get(p)); !errors.Is(err, dpop.ErrBadSignature) {
		t.Fatalf("bad sig + no nonce = %v, want ErrBadSignature", err)
	}
	// No nonce and a wrong htu → the nonce is reported (and no jti spent).
	cl := claims(c, "")
	cl.HTU = testOrigin + "/elsewhere"
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); !errors.Is(err, dpop.ErrUseNonce) {
		t.Fatalf("no nonce + wrong htu = %v, want ErrUseNonce", err)
	}
	cl.Nonce = nonce
	cl.HTU = testOrigin + "/r"
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); err != nil {
		t.Fatalf("the jti refused by the nonce check was spent: %v", err)
	}
	// Replay with a wrong htu → replay.
	cl.HTU = testOrigin + "/elsewhere"
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("replay + wrong htu = %v, want ErrReplay", err)
	}
	// Wrong htu and wrong ath → htu.
	cl2 := claims(c, nonce)
	cl2.JTI = "second"
	cl2.HTU = testOrigin + "/elsewhere"
	cl2.ATH = "wrong"
	req := get(sign(t, k, cl2))
	req.AccessToken, req.Binding = "tok", other.Thumbprint()
	if _, err := v.Verify(ctx, req); !errors.Is(err, dpop.ErrHTUMismatch) {
		t.Fatalf("wrong htu + ath = %v, want ErrHTUMismatch", err)
	}
	// Wrong ath and wrong binding → ath.
	cl3 := claims(c, nonce)
	cl3.JTI = "third"
	cl3.ATH = "wrong"
	req = get(sign(t, k, cl3))
	req.AccessToken, req.Binding = "tok", other.Thumbprint()
	if _, err := v.Verify(ctx, req); !errors.Is(err, dpop.ErrATHMismatch) {
		t.Fatalf("wrong ath + binding = %v, want ErrATHMismatch", err)
	}
}

func TestVerify_ReplayRefused(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Minute)
	k := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	ctx := context.Background()

	// A second proof reusing a jti is a replay, even inside the reuse window.
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	cl := claims(c, nonce)
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("new proof, same jti = %v, want ErrReplay", err)
	}
	// With reuse off, even the same proof again is a replay.
	off := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now; cfg.ReuseWindow = -1 })
	c.Advance(5 * time.Second) // away from the bucket edge, so stepping back keeps the nonce valid
	p := sign(t, k, claims(c, nonce))
	if _, err := off.Verify(ctx, get(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := off.Verify(ctx, get(p)); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("reuse off, same proof = %v, want ErrReplay", err)
	}
	// Nor when the verifier's clock steps backwards.
	c.Advance(-time.Second)
	if _, err := off.Verify(ctx, get(p)); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("reuse off, same proof, clock stepped back = %v, want ErrReplay", err)
	}
	c.Advance(time.Second)
	// Until the nonce expires the jti is remembered.
	c.Advance(90 * time.Second)
	if _, err := v.Verify(ctx, get(sign(t, k, cl))); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("same jti after 90 s with a still-valid nonce = %v, want ErrReplay", err)
	}
}

// The same proof is accepted again while its window lasts, and refused after.
func TestVerify_ReuseWindow(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Hour)
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	p := sign(t, k, claims(c, nonce))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(ctx, get(p)); err != nil {
			t.Fatalf("presentation %d: %v", i, err)
		}
		c.Advance(10 * time.Second)
	}
	// 30 s after the first presentation the window is closed.
	if _, err := v.Verify(ctx, get(p)); !errors.Is(err, dpop.ErrReplay) {
		t.Fatalf("after the window = %v, want ErrReplay", err)
	}
}

// A reused proof still passes the nonce and target checks: the cache saves
// only the signature verification.
func TestVerify_ReuseStillChecksNonceAndTarget(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Second) // nonces live 1–2 s
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	p := sign(t, k, claims(c, nonce))
	ctx := context.Background()
	if _, err := v.Verify(ctx, get(p)); err != nil {
		t.Fatal(err)
	}
	other := get(p)
	other.Path = "/other"
	if _, err := v.Verify(ctx, other); !errors.Is(err, dpop.ErrHTUMismatch) {
		t.Fatalf("reused proof on another path = %v, want ErrHTUMismatch", err)
	}
	c.Advance(3 * time.Second)
	if _, err := v.Verify(ctx, get(p)); !errors.Is(err, dpop.ErrUseNonce) {
		t.Fatalf("reused proof after its nonce expired = %v, want ErrUseNonce", err)
	}
}

// The signature cache is keyed by the hash of the whole proof: a proof that
// shares the jti and key of a cached one but differs in any byte is verified
// afresh.
func TestVerify_SignatureCacheKeyedByProofHash(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Hour)
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	p := sign(t, k, claims(c, nonce))
	ctx := context.Background()
	if _, err := v.Verify(ctx, get(p)); err != nil {
		t.Fatal(err)
	}
	forged := rewrite(t, p, nil, func(m map[string]any) { m["htm"] = "DELETE" })
	req := get(forged)
	req.Method = "DELETE"
	if _, err := v.Verify(ctx, req); !errors.Is(err, dpop.ErrBadSignature) {
		t.Fatalf("forged claims under a cached proof's signature = %v, want ErrBadSignature", err)
	}
}

func TestVerify_ClockSkewIrrelevantWithNonces(t *testing.T) {
	c := newClock()
	nonces := hmacNonces(t, c, time.Minute)
	v := newVerifier(t, nonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	nonce, _ := nonces.Issue(k.Thumbprint())
	for i, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		cl := claims(c, nonce)
		cl.JTI = string(rune('a' + i))
		cl.IAT = c.Now().Add(skew).Unix()
		if _, err := v.Verify(context.Background(), get(sign(t, k, cl))); err != nil {
			t.Fatalf("iat skewed by %s with a valid nonce: %v", skew, err)
		}
	}
}

func TestVerify_IATWindowWithoutNonces(t *testing.T) {
	c := newClock()
	v := newVerifier(t, nil, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	for i, tc := range []struct {
		skew time.Duration
		ok   bool
	}{{-59 * time.Second, true}, {59 * time.Second, true}, {-61 * time.Second, false}, {61 * time.Second, false}} {
		cl := claims(c, "")
		cl.JTI = string(rune('a' + i))
		cl.IAT = c.Now().Add(tc.skew).Unix()
		_, err := v.Verify(context.Background(), get(sign(t, k, cl)))
		if tc.ok && err != nil {
			t.Fatalf("iat %s: %v", tc.skew, err)
		}
		if !tc.ok && !errors.Is(err, dpop.ErrStaleProof) {
			t.Fatalf("iat %s = %v, want ErrStaleProof", tc.skew, err)
		}
	}
}

// A gRPC call is POST to origin + full method name.
func TestVerify_GRPCTarget(t *testing.T) {
	k := mustKey(t)
	p, err := dpop.NewProof(k, "POST", testOrigin+"/pkg.v1.Things/Get", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newVerifier(t, nil)
	if _, err := v.Verify(context.Background(), dpop.Request{Method: "POST", Path: "/pkg.v1.Things/Get", Proofs: []string{p}}); err != nil {
		t.Fatal(err)
	}
}

// The htu comparison normalises scheme and host case and the default port,
// and ignores query and fragment, on both sides.
func TestVerify_HTUNormalisation(t *testing.T) {
	c := newClock()
	v := newVerifier(t, nil, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	for i, htu := range []string{
		"https://API.Example.com/r", "HTTPS://api.example.com:443/r", "https://api.example.com/r?q=1#f",
	} {
		cl := claims(c, "")
		cl.JTI = string(rune('a' + i))
		cl.HTU = htu
		req := get(sign(t, k, cl))
		req.Path = "/r?x=y"
		if _, err := v.Verify(context.Background(), req); err != nil {
			t.Fatalf("htu %s: %v", htu, err)
		}
	}
}

func TestNormalizeHTU(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.example/p":         "https://a.example/p",
		"https://a.example":           "https://a.example/",
		"http://a.example:80/p":       "http://a.example/p",
		"https://a.example:8443/p?q":  "https://a.example:8443/p",
		"https://[::1]:443/p":         "https://[::1]/p",
		"https://a.example/a%2Fb#top": "https://a.example/a%2Fb",
	} {
		got, err := dpop.NormalizeHTU(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHTU(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/p", "ftp://a/p", "https://user@a/p", "https:///p", "::"} {
		if _, err := dpop.NormalizeHTU(bad); err == nil {
			t.Errorf("NormalizeHTU(%q) accepted", bad)
		}
	}
}

func TestNewVerifier_Config(t *testing.T) {
	for _, cfg := range []dpop.Config{
		{},
		{Origin: "api.example.com"},
		{Origin: testOrigin + "/base"},
		{Origin: testOrigin + "/base/"},
		{Origin: testOrigin, ReuseWindow: dpop.MaxReuseWindow + time.Second},
		{Origin: testOrigin, IATWindow: -time.Second},
	} {
		if _, err := dpop.NewVerifier(cfg); err == nil {
			t.Errorf("NewVerifier(%+v) accepted", cfg)
		}
	}
	for _, cfg := range []dpop.Config{
		{Origin: testOrigin},
		{Origin: testOrigin + "/"},
		{Origin: testOrigin, ReuseWindow: dpop.MaxReuseWindow},
		{Origin: testOrigin, ReuseWindow: -1},
	} {
		if _, err := dpop.NewVerifier(cfg); err != nil {
			t.Errorf("NewVerifier(%+v): %v", cfg, err)
		}
	}
	v := newVerifier(t, nil)
	if _, err := v.Nonce("x"); err == nil {
		t.Error("Nonce with nonces off succeeded")
	}
}

// A full jti store makes the verifier fail closed with a server error, not a
// refusal, and never accept unrecorded.
func TestVerify_FullJTIStoreFailsClosed(t *testing.T) {
	c := newClock()
	store := dpop.NewMemoryJTIStore(1)
	v := newVerifier(t, nil, func(cfg *dpop.Config) { cfg.Now = c.Now; cfg.JTIs = store })
	k := mustKey(t)
	cl := claims(c, "")
	if _, err := v.Verify(context.Background(), get(sign(t, k, cl))); err != nil {
		t.Fatal(err)
	}
	cl.JTI = "another"
	_, err := v.Verify(context.Background(), get(sign(t, k, cl)))
	if !errors.Is(err, dpop.ErrJTIStoreFull) {
		t.Fatalf("full store = %v, want ErrJTIStoreFull", err)
	}
	// Once the first jti expires (2 × iat window without nonces) there is room.
	c.Advance(2*dpop.DefaultIATWindow + time.Second)
	cl.JTI = "third"
	cl.IAT = c.Now().Unix()
	if _, err := v.Verify(context.Background(), get(sign(t, k, cl))); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("store holds %d entries, want 1", store.Len())
	}
}

func TestVerify_ConcurrentSameProofOneWinnerWithoutReuse(t *testing.T) {
	c := newClock()
	v := newVerifier(t, nil, func(cfg *dpop.Config) { cfg.Now = c.Now; cfg.ReuseWindow = -1 })
	k := mustKey(t)
	p := sign(t, k, claims(c, ""))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), get(p)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d concurrent presentations accepted, want 1", ok)
	}
}
