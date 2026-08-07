package txregistry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
	"google.golang.org/grpc/metadata"
)

// TestAdoptTx_UnknownIDIsLoud is the silent arm of T2-6 pass #8 D-F4.
//
// A request that carries `w17-tx-id` is a request whose caller believes it
// is inside someone else's transaction. When the id resolves to nothing in
// THIS bundle's registry, the old behaviour opened a fresh transaction and
// committed it independently — the caller's later Rollback then rolled back
// an empty transaction somewhere else and nothing anywhere reported a
// problem. That is silent atomicity loss: the write survives a rollback.
//
// The id can be unknown here for exactly one interesting reason — it was
// minted by a DIFFERENT registry (another bundle's coordinator). The
// registry cannot tell that apart from a stale id, and it must not have to:
// either way the caller asked to join a transaction this process cannot
// join, and the honest answer is an error, not a different transaction.
//
// The generated call site already surfaces the error as
// `codes.InvalidArgument` ("tx adoption: …"), so no template changes.
func TestAdoptTx_UnknownIDIsLoud(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-minted-by-another-bundle"))
	tx, ok, release, err := txregistry.AdoptTx(ctx, fakeRegistry{}, "main")
	defer release()
	if err == nil {
		t.Fatalf("AdoptTx(unknown id) = (%v, %v, nil) — an unjoinable transaction must not silently become a fresh independent one", tx, ok)
	}
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("AdoptTx(unknown id) err = %v, want ErrUnknownTxID", err)
	}
	if ok || tx != nil {
		t.Errorf("AdoptTx(unknown id) = (%v, %v, _); want (nil, false, err)", tx, ok)
	}
}

// TestAdoptTx_NoHeaderStaysSilent is the other half of the same rule: a
// request with NO `w17-tx-id` claims nothing, so opening a fresh per-method
// transaction is the correct, non-surprising answer. Only an id that was
// asserted and cannot be honoured is an error.
func TestAdoptTx_NoHeaderStaysSilent(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	tx, ok, release, err := txregistry.AdoptTx(ctx, fakeRegistry{}, "main")
	defer release()
	if tx != nil || ok || err != nil {
		t.Errorf("AdoptTx(no header) = (%v, %v, %v); want (nil, false, nil)", tx, ok, err)
	}
}
