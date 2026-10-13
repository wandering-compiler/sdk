package dpopstore_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// harness is one backend under the shared contract: a fresh store with the
// given per-key share, and a way to move time forward for both the records'
// FirstSeen and, where the backend expires by its own clock, the backend.
type harness struct {
	newStore func(t *testing.T, maxPerKey int) dpop.JTIStore
	advance  func(t *testing.T, d time.Duration)
}

// runContract runs every guarantee a shared JTIStore owes the verifier.
func runContract(t *testing.T, h harness) {
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)

	t.Run("insert if absent", func(t *testing.T) {
		s := h.newStore(t, 0)
		rec := dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h1", FirstSeen: base}
		got, created, err := s.Record(ctx, "tp\x00j1", rec, time.Minute)
		if err != nil || !created || got != rec {
			t.Fatalf("first Record = (%+v, %v, %v), want created", got, created, err)
		}
		other := dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h2", FirstSeen: base.Add(time.Second)}
		got, created, err = s.Record(ctx, "tp\x00j1", other, time.Minute)
		if err != nil || created {
			t.Fatalf("second Record = (%v, %v), want the stored record", created, err)
		}
		if got.ProofHash != "h1" || got.Thumbprint != "tp" || !got.FirstSeen.Equal(base) {
			t.Errorf("stored record = %+v, want the first one", got)
		}
	})

	t.Run("expiry frees the key", func(t *testing.T) {
		s := h.newStore(t, 0)
		ttl := 200 * time.Millisecond
		rec := dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h1", FirstSeen: base}
		if _, created, err := s.Record(ctx, "tp\x00j", rec, ttl); err != nil || !created {
			t.Fatalf("Record: %v %v", created, err)
		}
		h.advance(t, ttl+50*time.Millisecond)
		again := dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h2", FirstSeen: base.Add(ttl + 50*time.Millisecond)}
		got, created, err := s.Record(ctx, "tp\x00j", again, ttl)
		if err != nil || !created || got.ProofHash != "h2" {
			t.Fatalf("after expiry = (%+v, %v, %v), want a fresh record", got, created, err)
		}
	})

	t.Run("per-key share", func(t *testing.T) {
		s := h.newStore(t, 3)
		ttl := 200 * time.Millisecond
		for i := range 3 {
			rec := dpop.JTIRecord{Thumbprint: "greedy", ProofHash: fmt.Sprint(i), FirstSeen: base}
			if _, created, err := s.Record(ctx, fmt.Sprintf("greedy\x00%d", i), rec, ttl); err != nil || !created {
				t.Fatalf("entry %d: %v %v", i, created, err)
			}
		}
		_, _, err := s.Record(ctx, "greedy\x00x", dpop.JTIRecord{Thumbprint: "greedy", ProofHash: "x", FirstSeen: base}, ttl)
		if !errors.Is(err, dpop.ErrJTIStoreFull) {
			t.Fatalf("over the share: %v, want ErrJTIStoreFull", err)
		}
		if _, created, err := s.Record(ctx, "polite\x00a", dpop.JTIRecord{Thumbprint: "polite", ProofHash: "a", FirstSeen: base}, ttl); err != nil || !created {
			t.Fatalf("another key was refused: %v %v", created, err)
		}
		h.advance(t, ttl+50*time.Millisecond)
		later := base.Add(ttl + 50*time.Millisecond)
		if _, created, err := s.Record(ctx, "greedy\x00y", dpop.JTIRecord{Thumbprint: "greedy", ProofHash: "y", FirstSeen: later}, ttl); err != nil || !created {
			t.Fatalf("the share did not free with expiry: %v %v", created, err)
		}
	})

	t.Run("concurrent writers, one winner", func(t *testing.T) {
		s := h.newStore(t, 0)
		const n = 16
		var wg sync.WaitGroup
		results := make([]dpop.JTIRecord, n)
		createds := make([]bool, n)
		errs := make([]error, n)
		for i := range n {
			wg.Go(func() {
				rec := dpop.JTIRecord{Thumbprint: "tp", ProofHash: fmt.Sprint("h", i), FirstSeen: base.Add(time.Duration(i) * time.Microsecond)}
				results[i], createds[i], errs[i] = s.Record(ctx, "tp\x00race", rec, time.Minute)
			})
		}
		wg.Wait()
		winners := 0
		var winner string
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("writer %d: %v", i, errs[i])
			}
			if createds[i] {
				winners++
				winner = results[i].ProofHash
			}
		}
		if winners != 1 {
			t.Fatalf("%d writers created the same jti, want exactly 1", winners)
		}
		for i := range n {
			if results[i].ProofHash != winner {
				t.Errorf("writer %d saw %q, want the winner's %q", i, results[i].ProofHash, winner)
			}
		}
	})

	t.Run("replicas sharing the store refuse a replay", func(t *testing.T) {
		bound, mint := h.newStore(t, 0), h.newStore(t, 0)
		replica := func() *dpop.Verifier {
			v, err := dpop.NewVerifier(dpop.Config{Origin: "https://api.example.com", JTIs: bound, MintJTIs: mint, ReuseWindow: -1})
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		a, b := replica(), replica()
		key, err := dpop.GenerateKey(dpop.KeyP256)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := dpop.NewProof(key, http.MethodGet, "https://api.example.com/v1/notes", "tok", "")
		if err != nil {
			t.Fatal(err)
		}
		req := dpop.Request{Method: http.MethodGet, Path: "/v1/notes", Proofs: []string{proof}, AccessToken: "tok", Binding: key.Thumbprint()}
		if _, err := a.Verify(ctx, req); err != nil {
			t.Fatalf("replica A: %v", err)
		}
		if _, err := b.Verify(ctx, req); !errors.Is(err, dpop.ErrReplay) {
			t.Fatalf("replica B accepted a proof replica A already accepted: %v", err)
		}
	})
}
