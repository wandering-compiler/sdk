package kvfs_test

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

func TestBucketDepth(t *testing.T) {
	cases := []struct {
		name         string
		expected     uint64
		maxPerBucket uint32
		wantDepth    int
	}{
		{name: "empty", expected: 0, maxPerBucket: 1000, wantDepth: 0},
		{name: "below cap", expected: 500, maxPerBucket: 1000, wantDepth: 0},
		{name: "at cap", expected: 1000, maxPerBucket: 1000, wantDepth: 0},
		// 100k objects, 1k per bucket → ratio 100 → depth 1
		// (256^1 = 256 buckets; 1000*256 = 256k capacity ≥ 100k).
		{name: "100k / 1k", expected: 100_000, maxPerBucket: 1000, wantDepth: 1},
		// 1M objects, 1k per bucket → ratio 1000 → depth 2
		// (256^1=256 < 1000; 256^2=65536 ≥ 1000).
		{name: "1m / 1k", expected: 1_000_000, maxPerBucket: 1000, wantDepth: 2},
		// 1B objects, 1k per bucket → ratio 1M → depth 3.
		{name: "1b / 1k", expected: 1_000_000_000, maxPerBucket: 1000, wantDepth: 3},
		// Pathological max=1: depth 1 fallback.
		{name: "max=1 fallback", expected: 100, maxPerBucket: 1, wantDepth: 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := kvfs.BucketDepth(tc.expected, tc.maxPerBucket)
			if got != tc.wantDepth {
				t.Errorf("BucketDepth(%d, %d) = %d, want %d", tc.expected, tc.maxPerBucket, got, tc.wantDepth)
			}
		})
	}
}

func TestBuildKey_FlatLayout(t *testing.T) {
	// expected ≤ maxPerBucket → depth 0 → no sub-buckets.
	got := kvfs.BuildKey("/avatars", "abc123", 100, 1000)
	want := "/avatars/abc123"
	if got != want {
		t.Errorf("BuildKey flat: got %q, want %q", got, want)
	}
}

func TestBuildKey_OneLevel(t *testing.T) {
	got := kvfs.BuildKey("/avatars", "alice", 100_000, 1000)
	// First 2 hex chars of sha256("alice").
	if !strings.HasPrefix(got, "/avatars/") {
		t.Errorf("missing bucket prefix: %q", got)
	}
	parts := strings.Split(strings.TrimPrefix(got, "/"), "/")
	// "avatars" / "<2-hex>" / "alice"
	if len(parts) != 3 {
		t.Fatalf("expected 3 path segments, got %d: %q", len(parts), got)
	}
	if len(parts[1]) != 2 {
		t.Errorf("sub-bucket segment length %d, want 2: %q", len(parts[1]), parts[1])
	}
	if parts[2] != "alice" {
		t.Errorf("object key segment = %q, want alice", parts[2])
	}
}

func TestBuildKey_TwoLevels(t *testing.T) {
	got := kvfs.BuildKey("/avatars", "bob", 1_000_000, 1000)
	parts := strings.Split(strings.TrimPrefix(got, "/"), "/")
	// "avatars" / "<2-hex>" / "<2-hex>" / "bob"
	if len(parts) != 4 {
		t.Fatalf("expected 4 path segments, got %d: %q", len(parts), got)
	}
	if len(parts[1]) != 2 || len(parts[2]) != 2 {
		t.Errorf("sub-bucket segment lengths = %d/%d, want 2/2: %q/%q", len(parts[1]), len(parts[2]), parts[1], parts[2])
	}
}

func TestBuildKey_NormalisesPrefix(t *testing.T) {
	cases := []struct {
		bucketPath string
		want       string
	}{
		{bucketPath: "/avatars", want: "/avatars/k"},
		{bucketPath: "avatars", want: "/avatars/k"},
		{bucketPath: "/avatars/", want: "/avatars/k"},
		{bucketPath: "", want: "k"},
		{bucketPath: "/", want: "k"},
	}
	for _, tc := range cases {
		got := kvfs.BuildKey(tc.bucketPath, "k", 1, 1000)
		if got != tc.want {
			t.Errorf("BuildKey(%q, k) = %q, want %q", tc.bucketPath, got, tc.want)
		}
	}
}

// TestBuildKey_Distribution feeds 1000 random keys at depth 1
// (256 sub-buckets). A perfectly uniform sha256 distribution
// puts ~3.9 keys per bucket; we assert the empirical max
// stays under a generous safety factor (×4) to catch
// regressions in the bucketing logic without being flaky on
// small samples.
func TestBuildKey_Distribution(t *testing.T) {
	const (
		nKeys      = 1000
		expected   = 100_000 // depth 1
		maxBucket  = 1000
		bucketRoot = "/uploads"
	)
	counts := map[string]int{}
	for i := 0; i < nKeys; i++ {
		var rnd [16]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		key := hex.EncodeToString(rnd[:])
		built := kvfs.BuildKey(bucketRoot, key, expected, maxBucket)
		// Sub-bucket is the segment between bucketRoot and
		// the object key.
		parts := strings.Split(strings.TrimPrefix(built, bucketRoot+"/"), "/")
		if len(parts) < 2 {
			t.Fatalf("unexpected layout: %q", built)
		}
		counts[parts[0]]++
	}
	// Expected per-bucket = 1000 / 256 ≈ 3.9. Cap at 4× that
	// (16) — very loose for 1000 samples but still catches
	// "all keys in one bucket" regressions.
	maxObserved := 0
	for _, c := range counts {
		if c > maxObserved {
			maxObserved = c
		}
	}
	if maxObserved > 16 {
		t.Errorf("max bucket size = %d (want ≤ 16); distribution is suspect", maxObserved)
	}
	// At least 200 buckets should be touched out of 256.
	// (Coupon-collector argument; 1000 draws into 256 bins
	// hits ~256-256*(1-1/256)^1000 ≈ 256 - 256*0.0192 ≈ 251
	// expected; floor at 200 for a comfortable margin.)
	if len(counts) < 200 {
		t.Errorf("touched bucket count = %d (want ≥ 200); distribution is non-uniform", len(counts))
	}
}

func TestHashKey(t *testing.T) {
	got := kvfs.HashKey([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("HashKey(\"hello\") = %q, want %q", got, want)
	}
}

func TestHashFromReader(t *testing.T) {
	got, n, err := kvfs.HashFromReader(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("HashFromReader: %v", err)
	}
	if n != 5 {
		t.Errorf("HashFromReader bytes = %d, want 5", n)
	}
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("HashFromReader hex = %q, want %q", got, want)
	}
}
