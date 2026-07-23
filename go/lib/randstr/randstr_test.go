package randstr_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/randstr"
)

// base64URLAlphabet is the RFC 4648 §5 URL-safe alphabet. Token
// output must be a substring of this set.
const base64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func TestToken_LengthInvariant(t *testing.T) {
	cases := []int{1, 8, 16, 31, 32, 33, 48, 63, 64, 65, 96, 128, 200}
	for _, n := range cases {
		got, err := randstr.Token(n)
		if err != nil {
			t.Errorf("Token(%d): %v", n, err)
			continue
		}
		if len(got) != n {
			t.Errorf("Token(%d) length = %d, want %d", n, len(got), n)
		}
	}
}

func TestToken_CharsetInvariant(t *testing.T) {
	// Pull 256 chars worth of output to give the alphabet check
	// real coverage — the test asserts every byte lands in the
	// base64url set; one-shot non-alphabet bytes would fail loudly.
	tok, err := randstr.Token(256)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	for i, r := range tok {
		if !strings.ContainsRune(base64URLAlphabet, r) {
			t.Errorf("Token[%d] = %q (rune %d) not in base64url alphabet", i, string(r), r)
		}
	}
}

func TestToken_NoPaddingInTruncated(t *testing.T) {
	// base64url-no-padding never emits `=`; the truncation
	// shouldn't reintroduce it either. Sweep lengths near the
	// 4-char boundary where padding would otherwise appear.
	for _, n := range []int{3, 5, 6, 7, 9, 10, 11, 13, 14, 15} {
		tok, err := randstr.Token(n)
		if err != nil {
			t.Errorf("Token(%d): %v", n, err)
			continue
		}
		if strings.Contains(tok, "=") {
			t.Errorf("Token(%d) = %q contains padding char", n, tok)
		}
	}
}

func TestToken_NonRepeating(t *testing.T) {
	// Sanity smoke — two consecutive 32-char tokens must differ
	// with overwhelming probability (collision odds ~2^-192).
	// A repeat here means crypto/rand is broken or the encoding
	// is constant.
	a, err := randstr.Token(32)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	b, err := randstr.Token(32)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if a == b {
		t.Errorf("two Token(32) calls returned identical output %q", a)
	}
}

func TestToken_RejectsNonPositive(t *testing.T) {
	cases := []int{0, -1, -100}
	for _, n := range cases {
		_, err := randstr.Token(n)
		if err == nil {
			t.Errorf("Token(%d) should error", n)
		}
	}
}
