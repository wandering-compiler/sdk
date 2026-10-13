package dpop_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// A nonce is valid in the bucket it was issued in and the next one, then
// never again; it is bound to its thumbprint and cannot be altered.
func TestHMACNonces_RotationAcrossBuckets(t *testing.T) {
	c := newClock()
	n := hmacNonces(t, c, time.Minute)
	nonce, err := n.Issue("tp")
	if err != nil {
		t.Fatal(err)
	}
	if !n.Valid(nonce, "tp") {
		t.Fatal("fresh nonce invalid")
	}
	if n.Valid(nonce, "other") {
		t.Fatal("nonce valid for another thumbprint")
	}
	raw := []byte(nonce)
	raw[len(raw)-2] ^= 1
	if n.Valid(string(raw), "tp") {
		t.Fatal("altered nonce valid")
	}
	c.Advance(time.Minute) // next bucket: still valid
	if !n.Valid(nonce, "tp") {
		t.Fatal("nonce of the previous bucket invalid")
	}
	next, _ := n.Issue("tp")
	if next == nonce {
		t.Fatal("a new bucket issued the same nonce")
	}
	c.Advance(time.Minute) // two buckets later: expired
	if n.Valid(nonce, "tp") {
		t.Fatal("nonce valid two buckets later")
	}
	if !n.Valid(next, "tp") {
		t.Fatal("rotated nonce invalid in its next bucket")
	}
	if n.Lifetime() != 2*time.Minute {
		t.Fatalf("Lifetime = %s", n.Lifetime())
	}
	// A nonce from the future (a bucket not reached yet) is invalid.
	c2 := newClock()
	c2.Advance(5 * time.Minute)
	future, _ := hmacNonces(t, c2, time.Minute).Issue("tp")
	if n.Valid(future, "tp") {
		t.Fatal("nonce from a future bucket valid")
	}
}

func TestHMACNonces_KeyRequirements(t *testing.T) {
	if _, err := dpop.NewHMACNonces(make([]byte, dpop.MinNonceKeyBytes-1)); err == nil {
		t.Fatal("short key accepted")
	}
	key := make([]byte, dpop.MinNonceKeyBytes)
	n, err := dpop.NewHMACNonces(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce, _ := n.Issue("tp")
	key[0] = 1 // the source keeps its own copy
	if !n.Valid(nonce, "tp") {
		t.Fatal("mutating the caller's key changed the source")
	}
	if n.Lifetime() != 2*dpop.DefaultNonceBucket {
		t.Fatalf("default lifetime %s", n.Lifetime())
	}
}

// Two replicas sharing the nonce key accept each other's nonces without
// shared state; a replica with another key does not.
func TestHMACNonces_SharedKeyAcrossReplicas(t *testing.T) {
	c := newClock()
	a := newVerifier(t, hmacNonces(t, c, time.Minute), func(cfg *dpop.Config) { cfg.Now = c.Now })
	b := newVerifier(t, hmacNonces(t, c, time.Minute), func(cfg *dpop.Config) { cfg.Now = c.Now })
	otherNonces, _ := dpop.NewHMACNonces([]byte("ffffffffffffffffffffffffffffffff"), dpop.WithNonceClock(c.Now))
	stranger := newVerifier(t, otherNonces, func(cfg *dpop.Config) { cfg.Now = c.Now })
	k := mustKey(t)
	ctx := context.Background()

	_, err := a.Verify(ctx, get(sign(t, k, claims(c, ""))))
	var ne *dpop.NonceError
	if !errors.As(err, &ne) {
		t.Fatalf("replica A without nonce = %v", err)
	}
	cl := claims(c, ne.Nonce)
	cl.JTI = "on-b"
	if _, err := b.Verify(ctx, get(sign(t, k, cl))); err != nil {
		t.Fatalf("replica B refused replica A's nonce: %v", err)
	}
	c.Advance(time.Minute) // rotation: still accepted in the next bucket on both
	cl.JTI = "on-a-later"
	if _, err := a.Verify(ctx, get(sign(t, k, cl))); err != nil {
		t.Fatalf("replica A one bucket later: %v", err)
	}
	cl.JTI = "on-stranger"
	if _, err := stranger.Verify(ctx, get(sign(t, k, cl))); !errors.Is(err, dpop.ErrUseNonce) {
		t.Fatalf("replica with another key = %v, want ErrUseNonce", err)
	}
}

func TestMemoryJTIStore_RecordAndExpiry(t *testing.T) {
	s := dpop.NewMemoryJTIStore(0)
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	rec := dpop.JTIRecord{ProofHash: "h1", FirstSeen: t0}
	if got, created, err := s.Record(ctx, "k", rec, time.Minute); err != nil || !created || got != rec {
		t.Fatalf("first Record = %v, %v, %v", got, created, err)
	}
	later := dpop.JTIRecord{ProofHash: "h2", FirstSeen: t0.Add(59 * time.Second)}
	if got, created, _ := s.Record(ctx, "k", later, time.Minute); created || got != rec {
		t.Fatalf("Record inside ttl = %v, %v; want the stored record", got, created)
	}
	expired := dpop.JTIRecord{ProofHash: "h3", FirstSeen: t0.Add(time.Minute)}
	if got, created, _ := s.Record(ctx, "k", expired, time.Minute); !created || got != expired {
		t.Fatalf("Record after ttl = %v, %v; want a new record", got, created)
	}
}

func TestChallenge_Headers(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{dpop.ErrMissingProof, `DPoP algs="ES256 EdDSA"`},
		{&dpop.NonceError{Nonce: "n1"}, `DPoP error="use_dpop_nonce", algs="ES256 EdDSA"`},
		{dpop.ErrBadSignature, `DPoP error="invalid_dpop_proof", algs="ES256 EdDSA"`},
		{dpop.ErrReplay, `DPoP error="invalid_dpop_proof", algs="ES256 EdDSA"`},
		{dpop.ErrATHMismatch, `DPoP error="invalid_token", algs="ES256 EdDSA"`},
		{dpop.ErrBindingMismatch, `DPoP error="invalid_token", algs="ES256 EdDSA"`},
	}
	for _, tc := range cases {
		if got := dpop.WWWAuthenticate(tc.err); got != tc.want {
			t.Errorf("WWWAuthenticate(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
	h := http.Header{}
	dpop.SetChallenge(h, &dpop.NonceError{Nonce: "n1"})
	if h.Get("DPoP-Nonce") != "n1" || h.Get("Cache-Control") != "no-store" ||
		!dpop.IsUseNonceChallenge(h.Values("WWW-Authenticate")) {
		t.Fatalf("SetChallenge headers = %v", h)
	}
	h = http.Header{}
	dpop.SetChallenge(h, dpop.ErrReplay)
	if h.Get("DPoP-Nonce") != "" || dpop.IsUseNonceChallenge(h.Values("WWW-Authenticate")) {
		t.Fatalf("a non-nonce refusal carries a nonce challenge: %v", h)
	}
	md := dpop.ChallengeMetadata(&dpop.NonceError{Nonce: "n2"})
	if got := md.Get("dpop-nonce"); len(got) != 1 || got[0] != "n2" || !dpop.IsUseNonceChallenge(md.Get("www-authenticate")) {
		t.Fatalf("ChallengeMetadata = %v", md)
	}
}

func TestIsUseNonceChallenge(t *testing.T) {
	yes := []string{
		`DPoP error="use_dpop_nonce"`,
		`dpop error="use_dpop_nonce", error_description="fresh nonce"`,
		`Bearer realm="x", DPoP error="use_dpop_nonce"`,
		`DPoP algs="ES256", error=use_dpop_nonce`,
	}
	no := []string{
		``, `Bearer error="use_dpop_nonce"`, `DPoP error="invalid_dpop_proof"`, `XDPoP error="use_dpop_nonce"`,
	}
	for _, v := range yes {
		if !dpop.IsUseNonceChallenge([]string{v}) {
			t.Errorf("%q not recognised", v)
		}
	}
	for _, v := range no {
		if dpop.IsUseNonceChallenge([]string{v}) {
			t.Errorf("%q recognised", v)
		}
	}
}

func TestAccessToken(t *testing.T) {
	for in, want := range map[string]string{
		"DPoP abc": "abc", "dpop abc": "abc", "  DPoP   abc  ": "abc",
		"Bearer abc": "", "DPoP": "", "DPoP ": "", "": "",
	} {
		got, ok := dpop.AccessToken(in)
		if got != want || ok != (want != "") {
			t.Errorf("AccessToken(%q) = %q, %v", in, got, ok)
		}
	}
	if strings.Contains(dpop.WWWAuthenticate(dpop.ErrMalformed), "nonce") {
		t.Error("malformed proof answered with a nonce challenge")
	}
}
