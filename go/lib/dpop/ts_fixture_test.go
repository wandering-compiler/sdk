package dpop_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// testdata/ts_proofs.json holds proofs made by the TypeScript client in
// ts/dpop. Regenerate it from ts/dpop with `npm run fixture` (Node >= 20) and
// commit it with whatever change made that necessary; every run uses a new
// key, and this test asserts no particular values, only that the real
// verifier accepts what the TypeScript side signed.
type tsFixture struct {
	Origin     string          `json:"origin"`
	Nonce      string          `json:"nonce"`
	JWK        json.RawMessage `json:"jwk"`
	Thumbprint string          `json:"thumbprint"`
	Cases      []struct {
		Name   string `json:"name"`
		Method string `json:"method"`
		Path   string `json:"path"`
		Token  string `json:"token"`
		Proof  string `json:"proof"`
	} `json:"cases"`
	Thumbprints []struct {
		JWK        json.RawMessage `json:"jwk"`
		Thumbprint string          `json:"thumbprint"`
	} `json:"thumbprints"`
}

// fixedNonce accepts exactly the fixture's nonce, so the proofs verify no
// matter when the test runs: with a nonce source the verifier never looks at
// iat.
type fixedNonce string

func (n fixedNonce) Issue(string) (string, error) { return string(n), nil }
func (n fixedNonce) Valid(nonce, _ string) bool   { return nonce == string(n) }
func (fixedNonce) Lifetime() time.Duration        { return 5 * time.Minute }

func loadTSFixture(t *testing.T) tsFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/ts_proofs.json")
	if err != nil {
		t.Fatal(err)
	}
	var f tsFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 || len(f.Thumbprints) == 0 || f.Nonce == "" {
		t.Fatal("fixture holds no cases")
	}
	return f
}

func tsRequest(f tsFixture, i int) dpop.Request {
	c := f.Cases[i]
	r := dpop.Request{Method: c.Method, Path: c.Path, Proofs: []string{c.Proof}, AccessToken: c.Token}
	if c.Token != "" {
		r.Binding = f.Thumbprint
	}
	return r
}

// Every TypeScript proof verifies with the Go verifier and yields the
// thumbprint the TypeScript side computed.
func TestTSProofsVerify(t *testing.T) {
	f := loadTSFixture(t)
	v, err := dpop.NewVerifier(dpop.Config{Origin: f.Origin, Nonces: fixedNonce(f.Nonce)})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range f.Cases {
		tp, err := v.Verify(context.Background(), tsRequest(f, i))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if tp != f.Thumbprint {
			t.Fatalf("%s: thumbprint %s, TypeScript computed %s", c.Name, tp, f.Thumbprint)
		}
	}
}

// The same proofs are refused once anything they are bound to changes, so the
// test above cannot pass on a verifier that accepts anything.
func TestTSProofsRefusedWhenTheirBindingsChange(t *testing.T) {
	f := loadTSFixture(t)
	fresh := func(nonce string) *dpop.Verifier {
		v, err := dpop.NewVerifier(dpop.Config{Origin: f.Origin, Nonces: fixedNonce(nonce)})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for i, c := range f.Cases {
		check := func(what string, v *dpop.Verifier, r dpop.Request, want error) {
			t.Helper()
			if _, err := v.Verify(context.Background(), r); !errors.Is(err, want) {
				t.Errorf("%s, %s: err = %v, want %v", c.Name, what, err, want)
			}
		}
		r := tsRequest(f, i)
		check("other nonce", fresh("another-nonce"), r, dpop.ErrUseNonce)

		other := r
		other.Method = "PATCH"
		check("other method", fresh(f.Nonce), other, dpop.ErrHTMMismatch)

		other = r
		other.Path = "/elsewhere"
		check("other path", fresh(f.Nonce), other, dpop.ErrHTUMismatch)

		other = r
		other.AccessToken, other.Binding = "another-token", f.Thumbprint
		check("other token", fresh(f.Nonce), other, dpop.ErrATHMismatch)

		if c.Token != "" {
			other = r
			other.Binding = "another-thumbprint"
			check("other binding", fresh(f.Nonce), other, dpop.ErrBindingMismatch)
		}

		other = r
		parts := strings.Split(c.Proof, ".")
		sig := []byte(parts[2])
		if sig[5] == 'A' {
			sig[5] = 'B'
		} else {
			sig[5] = 'A'
		}
		other.Proofs = []string{parts[0] + "." + parts[1] + "." + string(sig)}
		check("signature changed", fresh(f.Nonce), other, dpop.ErrBadSignature)
	}
}

// The thumbprints the TypeScript side computed match dpop.Thumbprint.
func TestTSThumbprintsMatch(t *testing.T) {
	f := loadTSFixture(t)
	all := append([]struct {
		JWK        json.RawMessage `json:"jwk"`
		Thumbprint string          `json:"thumbprint"`
	}{{JWK: f.JWK, Thumbprint: f.Thumbprint}}, f.Thumbprints...)
	for _, tc := range all {
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(tc.JWK); err != nil {
			t.Fatalf("%s: %v", tc.JWK, err)
		}
		got, err := dpop.Thumbprint(k.Key)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.Thumbprint {
			t.Fatalf("%s: Go thumbprint %s, TypeScript %s", tc.JWK, got, tc.Thumbprint)
		}
	}
}
