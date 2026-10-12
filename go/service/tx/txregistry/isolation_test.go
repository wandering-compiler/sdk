package txregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// isoReg is a Memory whose transactions report a fixed opened level, so
// every level can be exercised on a driver that does not accept them all
// in BeginTx. Everything else, including the lease, is the real Memory.
type isoReg struct {
	*txregistry.Memory
	opened sql.IsolationLevel
}

func (r isoReg) IsolationFor(string) sql.IsolationLevel { return r.opened }

// blindReg is a registry that cannot say what level a transaction has.
type blindReg struct{ txregistry.Registry }

func TestAdoptTxIsolated_RefusesWeakerOrUnknownLevels(t *testing.T) {
	cases := []struct {
		name   string
		opened sql.IsolationLevel
		want   sql.IsolationLevel
		adopt  bool
	}{
		{"weaker", sql.LevelReadCommitted, sql.LevelSerializable, false},
		{"one step weaker", sql.LevelRepeatableRead, sql.LevelSerializable, false},
		{"unpinned", sql.LevelDefault, sql.LevelReadCommitted, false},
		{"unpinned against the weakest", sql.LevelDefault, sql.LevelReadUncommitted, false},
		{"a level the ranking does not know", sql.LevelSnapshot, sql.LevelReadCommitted, false},
		{"equal", sql.LevelRepeatableRead, sql.LevelRepeatableRead, true},
		{"stronger", sql.LevelSerializable, sql.LevelReadCommitted, true},
		{"linearizable satisfies serializable", sql.LevelLinearizable, sql.LevelSerializable, true},
		{"nothing declared", sql.LevelDefault, sql.LevelDefault, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem, _, id := leaseReg(t)
			reg := isoReg{Memory: mem, opened: c.opened}
			ctx := leaseCtx(id)

			tx, adopted, release, err := txregistry.AdoptTxIsolated(ctx, reg, "main", c.want)
			if c.adopt {
				if err != nil || !adopted || tx == nil {
					t.Fatalf("AdoptTxIsolated = tx %v, adopted %v, err %v; want the transaction", tx, adopted, err)
				}
				release()
				return
			}
			if !errors.Is(err, txregistry.ErrIsolationTooWeak) {
				t.Fatalf("err = %v, want ErrIsolationTooWeak", err)
			}
			if tx != nil || adopted {
				t.Fatalf("refusal handed out tx %v, adopted %v", tx, adopted)
			}
			release() // must be callable
			// The refusal gave the lease back: the transaction is still
			// adoptable and finishable.
			_, ok, rel, err := txregistry.AdoptTx(ctx, mem, "main")
			if err != nil || !ok {
				t.Fatalf("AdoptTx after the refusal = %v, %v; the refusal kept the lease", ok, err)
			}
			rel()
			if err := mem.Commit(context.Background(), id); err != nil {
				t.Fatalf("Commit after the refusal: %v", err)
			}
		})
	}
}

// A registry without IsolationReporter can never satisfy a declared level.
func TestAdoptTxIsolated_RegistryThatCannotReportRefuses(t *testing.T) {
	mem, _, id := leaseReg(t)
	_, _, _, err := txregistry.AdoptTxIsolated(leaseCtx(id), blindReg{mem}, "main", sql.LevelReadUncommitted)
	if !errors.Is(err, txregistry.ErrIsolationTooWeak) {
		t.Fatalf("err = %v, want ErrIsolationTooWeak", err)
	}
	if _, _, rel, err := txregistry.AdoptTx(leaseCtx(id), mem, "main"); err != nil {
		t.Fatalf("lease kept after the refusal: %v", err)
	} else {
		rel()
	}
}

// Memory reports the level the transaction was opened with.
func TestAdoptTxIsolated_MemoryReportsTheOpenedLevel(t *testing.T) {
	db := openSQLite(t)
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(0))
	unpinned, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		TxOptions:      &sql.TxOptions{Isolation: sql.LevelSerializable},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.IsolationFor(unpinned); got != sql.LevelDefault {
		t.Errorf("unpinned IsolationFor = %v, want Default", got)
	}
	if got := reg.IsolationFor(pinned); got != sql.LevelSerializable {
		t.Errorf("pinned IsolationFor = %v, want Serializable", got)
	}
	if got := reg.IsolationFor("not-an-id"); got != sql.LevelDefault {
		t.Errorf("unknown id IsolationFor = %v, want Default", got)
	}

	if _, _, _, err := txregistry.AdoptTxIsolated(leaseCtx(unpinned), reg, "main", sql.LevelSerializable); !errors.Is(err, txregistry.ErrIsolationTooWeak) {
		t.Errorf("unpinned transaction, declared Serializable: err = %v, want ErrIsolationTooWeak", err)
	}
	tx, ok, rel, err := txregistry.AdoptTxIsolated(leaseCtx(pinned), reg, "main", sql.LevelSerializable)
	if err != nil || !ok || tx == nil {
		t.Fatalf("pinned Serializable, declared Serializable: %v, %v", ok, err)
	}
	rel()
}

// The fresh-transaction path is unaffected: no id, no adoption, no error.
func TestAdoptTxIsolated_NoTransactionIdOpensFresh(t *testing.T) {
	reg := singleConnRegistry(t)
	tx, ok, rel, err := txregistry.AdoptTxIsolated(context.Background(), reg, "main", sql.LevelSerializable)
	if err != nil || ok || tx != nil {
		t.Fatalf("no id: tx %v, adopted %v, err %v; want the fresh path", tx, ok, err)
	}
	rel()
}
