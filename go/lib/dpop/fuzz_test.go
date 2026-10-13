package dpop_test

import (
	"context"
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
