package txregistry_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// TestMemory_Begin_BeginTxError covers Begin's db.BeginTx failure arm (incl. the
// cancel() cleanup of the timeout context): a closed *sql.DB can't start a tx.
func TestMemory_Begin_BeginTxError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // BeginTx now fails
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	// Timeout > 0 so the WithTimeout cancel-on-error branch is exercised too.
	if _, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main", Timeout: time.Second}); err == nil {
		t.Error("Begin on a closed DB should fail at BeginTx")
	}
}

// TestMemory_Commit_Error covers Commit's tx.Commit() failure arm: finalising
// the underlying *sql.Tx out-of-band (via LookupTx) makes the registry's later
// Commit return sql.ErrTxDone, exercising the best-effort-rollback + wrap path.
func TestMemory_Commit_Error(t *testing.T) {
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": openSQLite(t)})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, err := reg.LookupTx(id, "main")
	if err != nil {
		t.Fatalf("LookupTx: %v", err)
	}
	if err := tx.Rollback(); err != nil { // finalise the driver tx behind the registry's back
		t.Fatalf("direct rollback: %v", err)
	}
	if err := reg.Commit(id); err == nil {
		t.Error("Commit of an already-finalised tx should error (ErrTxDone)")
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after failed Commit = %d, want 0 (entry drained)", got)
	}
}

// TestMemory_Rollback_CancelsTimeout covers Rollback's cancel-branch: a tx begun
// with a Timeout carries a context cancel func that Rollback must release.
func TestMemory_Rollback_CancelsTimeout(t *testing.T) {
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": openSQLite(t)})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main", Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := reg.Rollback(id); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after Rollback = %d, want 0", got)
	}
}
