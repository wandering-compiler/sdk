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
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopstore"
)

// harness is one backend under the shared contract: a fresh store with the
// given per-key share and total cap (0: the defaults), and a way to move time forward for both the records'
// FirstSeen and, where the backend expires by its own clock, the backend.
type harness struct {
	newStore func(t *testing.T, maxPerKey, maxTotal int) dpop.JTIStore
	advance  func(t *testing.T, d time.Duration)
}

// runContract runs every guarantee a shared JTIStore owes the verifier.
func runContract(t *testing.T, h harness) {
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)

	t.Run("insert if absent", func(t *testing.T) {
		s := h.newStore(t, 0, 0)
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
		s := h.newStore(t, 0, 0)
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
		s := h.newStore(t, 3, 0)
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

	t.Run("total cap", func(t *testing.T) {
		s := h.newStore(t, 0, 3)
		ttl := 200 * time.Millisecond
		for i := range 3 {
			tp := fmt.Sprint("tp", i)
			if _, created, err := s.Record(ctx, tp+"\x00j", dpop.JTIRecord{Thumbprint: tp, ProofHash: tp, FirstSeen: base}, ttl); err != nil || !created {
				t.Fatalf("entry %d: %v %v", i, created, err)
			}
		}
		_, _, err := s.Record(ctx, "tp9\x00j", dpop.JTIRecord{Thumbprint: "tp9", ProofHash: "x", FirstSeen: base}, ttl)
		if !errors.Is(err, dpop.ErrJTIStoreFull) {
			t.Fatalf("over the total cap: %v, want ErrJTIStoreFull", err)
		}
		// A full store still answers for what it holds: a replay is a replay.
		got, created, err := s.Record(ctx, "tp1\x00j", dpop.JTIRecord{Thumbprint: "tp1", ProofHash: "other", FirstSeen: base}, ttl)
		if err != nil || created || got.ProofHash != "tp1" {
			t.Fatalf("stored key on a full store = (%+v, %v, %v), want the stored record", got, created, err)
		}
		// Room frees with expiry (the SQL store counts again within
		// SQLRecountEvery).
		wait := ttl + dpopstore.SQLRecountEvery
		h.advance(t, wait)
		if _, created, err := s.Record(ctx, "tp9\x00j", dpop.JTIRecord{Thumbprint: "tp9", ProofHash: "x", FirstSeen: base.Add(wait)}, ttl); err != nil || !created {
			t.Fatalf("the total cap did not free with expiry: %v %v", created, err)
		}
	})

	t.Run("a refused record takes no place under the total cap", func(t *testing.T) {
		s := h.newStore(t, 1, 2)
		rec := func(tp, jti string) error {
			_, _, err := s.Record(ctx, tp+"\x00"+jti, dpop.JTIRecord{Thumbprint: tp, ProofHash: jti, FirstSeen: base}, time.Minute)
			return err
		}
		if err := rec("a", "1"); err != nil {
			t.Fatal(err)
		}
		for i := range 5 {
			if err := rec("a", fmt.Sprint("more", i)); !errors.Is(err, dpop.ErrJTIStoreFull) {
				t.Fatalf("over the per-key share: %v, want ErrJTIStoreFull", err)
			}
		}
		if err := rec("b", "1"); err != nil {
			t.Fatalf("records refused by the per-key share took places under the total cap: %v", err)
		}
		if err := rec("c", "1"); !errors.Is(err, dpop.ErrJTIStoreFull) {
			t.Fatalf("over the total cap: %v, want ErrJTIStoreFull", err)
		}
	})

	t.Run("a mint flood fills only the mint store", func(t *testing.T) {
		const mintCap = 5
		bound, mint := h.newStore(t, 0, 0), h.newStore(t, 0, mintCap)
		verifier := func(policy dpop.MintFullPolicy) *dpop.Verifier {
			v, err := dpop.NewVerifier(dpop.Config{Origin: "https://api.example.com", JTIs: bound, MintJTIs: mint, MintFull: policy})
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		v := verifier(dpop.MintFullRefuse)
		unbound := func() dpop.Request {
			k, err := dpop.GenerateKey(dpop.KeyP256)
			if err != nil {
				t.Fatal(err)
			}
			p, err := dpop.NewProof(k, http.MethodPost, "https://api.example.com/v1/sign-in", "", "")
			if err != nil {
				t.Fatal(err)
			}
			return dpop.Request{Method: http.MethodPost, Path: "/v1/sign-in", Proofs: []string{p}}
		}
		for i := 0; ; i++ {
			_, err := v.Verify(ctx, unbound())
			if errors.Is(err, dpop.ErrUnavailable) {
				if !errors.Is(err, dpop.ErrJTIStoreFull) || i != mintCap {
					t.Fatalf("mint store refused after %d proofs with %v, want full after %d", i, err, mintCap)
				}
				break
			}
			if err != nil || i > mintCap {
				t.Fatalf("mint proof %d: %v", i, err)
			}
		}
		key, err := dpop.GenerateKey(dpop.KeyP256)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			proof, err := dpop.NewProof(key, http.MethodGet, "https://api.example.com/v1/notes", "tok", "")
			if err != nil {
				t.Fatal(err)
			}
			req := dpop.Request{Method: http.MethodGet, Path: "/v1/notes", Proofs: []string{proof}, AccessToken: "tok", Binding: key.Thumbprint()}
			if _, err := v.Verify(ctx, req); err != nil {
				t.Fatalf("bound proof %d during a mint flood: %v", i, err)
			}
		}
		r, err := verifier(dpop.MintFullAcceptUnrecorded).VerifyResult(ctx, unbound())
		if err != nil || !r.MintUnrecorded {
			t.Fatalf("accept-unrecorded on the full mint store = (%+v, %v)", r, err)
		}
	})

	t.Run("concurrent writers, one winner", func(t *testing.T) {
		s := h.newStore(t, 0, 0)
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
		bound, mint := h.newStore(t, 0, 0), h.newStore(t, 0, 0)
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
