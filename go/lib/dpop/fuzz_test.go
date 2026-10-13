package dpop_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// FuzzVerify feeds arbitrary proof strings to the verifier: it must never
// panic, and anything it accepts must carry a thumbprint. The seeds run as
// part of a plain `go test`.
func FuzzVerify(f *testing.F) {
	k, err := dpop.GenerateKey(dpop.KeyP256)
	if err != nil {
		f.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	good, err := dpop.SignClaims(k, dpop.Claims{JTI: "seed", HTM: "GET", HTU: testOrigin + "/r", IAT: now.Unix()})
	if err != nil {
		f.Fatal(err)
	}
	parts := strings.Split(good, ".")
	for _, seed := range []string{
		good,
		parts[0] + "." + parts[1] + ".",
		parts[0] + ".." + parts[2],
		"eyJhbGciOiJub25lIn0.e30.",
		"a.b.c",
		"...",
		"",
		strings.Repeat("A", 100),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, proof string) {
		v, err := dpop.NewVerifier(dpop.Config{Origin: testOrigin, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		tp, err := v.Verify(context.Background(), dpop.Request{Method: "GET", Path: "/r", Proofs: []string{proof}})
		if err == nil && tp == "" {
			t.Fatal("accepted a proof without a thumbprint")
		}
		if err != nil && tp != "" {
			t.Fatal("refused a proof but returned a thumbprint")
		}
	})
}

// FuzzNormalizeHTU: never panics, and a normalised URI is a fixed point.
func FuzzNormalizeHTU(f *testing.F) {
	for _, seed := range []string{
		"https://a.example/p", "HTTPS://A.example:443/p?q#f", "http://[::1]:80", "https://a.example:8443",
		"ftp://x", "//a/b", "https://user@a/p", "https://a.example/%zz", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out, err := dpop.NormalizeHTU(raw)
		if err != nil {
			return
		}
		again, err := dpop.NormalizeHTU(out)
		if err != nil || again != out {
			t.Fatalf("NormalizeHTU(%q) = %q, but NormalizeHTU(%q) = %q, %v", raw, out, out, again, err)
		}
	})
}

// FuzzVerifySigned reaches past the signature: the fuzzer mutates the
// protected-header and claims JSON, and each variant is signed with a test
// key before it is verified, so the claim checks see arbitrary input. The
// seeds are valid proofs. Whatever is accepted must be this key's, must
// match the request, and must not be accepted twice with reuse off.
func FuzzVerifySigned(f *testing.F) {
	k, err := dpop.GenerateKey(dpop.KeyEd25519)
	if err != nil {
		f.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	seed, err := dpop.SignClaims(k, dpop.Claims{JTI: "seed", HTM: "GET", HTU: testOrigin + "/r", IAT: now.Unix()})
	if err != nil {
		f.Fatal(err)
	}
	hdr := string(mustB64(f, strings.Split(seed, ".")[0]))
	iat := fmt.Sprint(now.Unix())
	ath := dpop.AccessTokenHash("tok")
	for _, claims := range []string{
		`{"jti":"a","htm":"GET","htu":"` + testOrigin + `/r","iat":` + iat + `}`,
		`{"jti":"b","htm":"GET","htu":"` + testOrigin + `/r","iat":` + iat + `,"ath":"` + ath + `"}`,
		`{"jti":"c","htm":"GET","htu":"HTTPS://API.EXAMPLE.COM:443/r?x#y","iat":` + iat + `,"nonce":"n"}`,
		`{"jti":"d","htm":"POST","htu":"` + testOrigin + `/r","iat":` + iat + `}`,
	} {
		f.Add(hdr, claims, false)
		f.Add(hdr, claims, true)
	}
	f.Fuzz(func(t *testing.T, header, claims string, withToken bool) {
		in := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
			base64.RawURLEncoding.EncodeToString([]byte(claims))
		sig, err := k.Sign(rand.Reader, []byte(in), crypto.Hash(0))
		if err != nil {
			t.Fatal(err)
		}
		proof := in + "." + base64.RawURLEncoding.EncodeToString(sig)
		v, err := dpop.NewVerifier(dpop.Config{Origin: testOrigin, ReuseWindow: -1, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		req := dpop.Request{Method: "GET", Path: "/r", Proofs: []string{proof}}
		if withToken {
			req.AccessToken, req.Binding = "tok", k.Thumbprint()
		}
		tp, err := v.Verify(context.Background(), req)
		if err != nil {
			return
		}
		if tp != k.Thumbprint() {
			t.Fatalf("accepted with thumbprint %s, the signing key is %s", tp, k.Thumbprint())
		}
		if _, err := v.Verify(context.Background(), req); !errors.Is(err, dpop.ErrReplay) {
			t.Fatalf("accepted proof presented again = %v, want ErrReplay", err)
		}
		var c map[string]any
		if json.Unmarshal([]byte(claims), &c) != nil || c["htm"] != "GET" {
			t.Fatalf("accepted claims %s", claims)
		}
		if _, hasATH := c["ath"]; hasATH != withToken {
			t.Fatalf("accepted claims %s with token=%v", claims, withToken)
		}
	})
}

func mustB64(f *testing.F, s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		f.Fatal(err)
	}
	return b
}
