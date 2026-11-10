package distx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// A file a business method stores with kvfs.Put is not part of its
// distributed transaction: Put commits on its own, so a transaction that
// rolls back afterwards leaves the object stored. Pinned because the spec
// states it as a deliberate limit (D13) — a change that started deferring
// or undoing it must update that statement.
func TestRun_AFileStoredInsideARolledBackTransactionStays(t *testing.T) {
	store := memory.New()
	var handle string
	refused := errors.New("the row write failed")
	err := distx.Run(context.Background(), &scriptedTx{}, mainReq(), func(ctx context.Context) error {
		h, err := kvfs.Put(ctx, store, kvfs.Placement{BucketPath: "/invoices"}, strings.NewReader("body"))
		if err != nil {
			return err
		}
		handle = h
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Run = %v", err)
	}
	if _, err := store.Stat(context.Background(), handle); err != nil {
		t.Errorf("Stat(%q) after the rollback = %v, want the object still stored", handle, err)
	}
}
