package migrate_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// mkUnadoptable is a pending migration with no adoption statements — the
// ALTER shape. `tablesIntroducedBy` is AddTable-only by design, so a
// migration that only alters an existing table introduces nothing a check
// can look for and carries neither preflight nor record body.
func mkUnadoptable(id, conn, body string) *applyfetchpb.Migration {
	m := mkMig(id, conn, body)
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(),
		m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), "", "")
	return m
}

// TestRunAdopt_MixedChainAdviceDoesNotRecommendPlainApply — T2-6 pass #10,
// B10-5.
//
// The unadoptable refusal named the ALTER-shaped migration as a cause and
// then advised "Such a connection does not NEED adopting — its migration
// collides with nothing — so apply it normally". For a chain of CREATE
// (adoptable) followed by ALTER (not), the hoisted check lists the ALTER
// and refuses the WHOLE connection — so following that advice runs the
// CREATE against a provisioned database and dies on `relation already
// exists`, the very failure adopt exists to end. Harmful, not merely
// incomplete: it points at an action that fails. And the mixed chain is
// not exotic — it is what every history looks like once one ALTER lands
// after a CREATE.
func TestRunAdopt_MixedChainAdviceDoesNotRecommendPlainApply(t *testing.T) {
	create := mkAdoptable("ts-1", "main", "CREATE TABLE t;")
	alter := mkUnadoptable("ts-2", "main", "ALTER TABLE t ADD COLUMN c text;")
	dir := seedDir(t, create, alter)

	db := stub.New()
	err := migrate.RunAdopt(context.Background(), migrate.Config{
		MigrationsDir: dir,
		Out:           io.Discard,
		Targets: targets(
			lockTarget{"main", create.GetId(), create.GetContentSha256()},
			lockTarget{"main", alter.GetId(), alter.GetContentSha256()},
		),
		AdoptConnections: []string{"main"},
		ApplierFor:       func(string) (migrate.Applier, error) { return db, nil },
	})
	if err == nil {
		t.Fatal("adopt accepted a chain containing an unadoptable migration")
	}
	msg := err.Error()
	if strings.Contains(msg, "apply it normally and narrow this command") {
		t.Error("the refusal still tells the operator to apply a mixed chain normally — that runs the CREATE against a database that already has the table")
	}
	for _, want := range []string{"MIXES", "Squash", "already exists"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name the real way through (missing %q):\n%s", want, msg)
		}
	}
	assertNothingRecorded(t, "main", db.Calls())
}

// TestRunAdopt_WhollyUnadoptableChainStillGetsTheApplyAdvice — the other
// direction. A connection whose ENTIRE pending chain is unadoptable really
// does collide with nothing (a KV keyspace comes into being when a key is
// written, so the body is comments), and "apply it normally" is the right
// answer there. A fix that replaced the advice everywhere would be just as
// wrong as the one that gave it everywhere.
func TestRunAdopt_WhollyUnadoptableChainStillGetsTheApplyAdvice(t *testing.T) {
	kv1 := mkUnadoptable("ts-1", "cache", "-- redis keyspace")
	kv2 := mkUnadoptable("ts-2", "cache", "-- redis keyspace")
	dir := seedDir(t, kv1, kv2)

	db := stub.New()
	err := migrate.RunAdopt(context.Background(), migrate.Config{
		MigrationsDir: dir,
		Out:           io.Discard,
		Targets: targets(
			lockTarget{"cache", kv1.GetId(), kv1.GetContentSha256()},
			lockTarget{"cache", kv2.GetId(), kv2.GetContentSha256()},
		),
		AdoptConnections: []string{"cache"},
		ApplierFor:       func(string) (migrate.Applier, error) { return db, nil },
	})
	if err == nil {
		t.Fatal("adopt accepted a wholly unadoptable chain")
	}
	if !strings.Contains(err.Error(), "apply it normally") {
		t.Errorf("a KV connection lost the advice that is correct for it:\n%v", err)
	}
	if strings.Contains(err.Error(), "Squash") {
		t.Errorf("a wholly unadoptable chain was told to squash, which it has no reason to do:\n%v", err)
	}
}

// TestRunAdopt_ResumesAfterAPartialRecord — T2-6 pass #10, D10-2.
//
// PASS 2 records one migration at a time, so a transport failure midway
// leaves the ledger at ts-k with the rest of the chain pending. Every other
// refusal on this path converges under a retry — Plan's applied-head cutoff
// drops what was recorded — but the non-empty-ledger guard did not, and the
// advice it gave was actively harmful: `migrate apply` runs the real DDL of
// a migration the preflight had just confirmed the database already holds.
// On PG that dead-ends in-tx; on MySQL, whose DDL is not transactional, a
// multi-statement body can partially execute first; and a non-colliding
// body (seed INSERTs, IF NOT EXISTS) applies successfully and re-runs its
// effects. Recovery was manual ledger surgery — the operation adopt exists
// to replace.
//
// What makes recording safe is the preflight, not an empty ledger.
func TestRunAdopt_ResumesAfterAPartialRecord(t *testing.T) {
	m1 := mkAdoptable("ts-1", "main", "CREATE TABLE a;")
	m2 := mkAdoptable("ts-2", "main", "CREATE TABLE b;")
	dir := seedDir(t, m1, m2)

	// Run 1: ts-1 records, ts-2's record write dies on the wire.
	first := stub.New()
	first.FailOn = m2.GetId()
	first.FailErr = errors.New("write tcp: connection reset by peer")
	cfg := func(a migrate.Applier) migrate.Config {
		return migrate.Config{
			MigrationsDir: dir,
			Out:           io.Discard,
			Targets: targets(
				lockTarget{"main", m1.GetId(), m1.GetContentSha256()},
				lockTarget{"main", m2.GetId(), m2.GetContentSha256()},
			),
			AdoptConnections: []string{"main"},
			ApplierFor:       func(string) (migrate.Applier, error) { return a, nil },
		}
	}
	if err := migrate.RunAdopt(context.Background(), cfg(first)); err == nil {
		t.Fatal("the seeded transport failure did not surface")
	}

	// Run 2: the operator retries. The ledger is now mid-chain at ts-1 and
	// ts-2 is still pending; the database holds both tables, which is what
	// the preflight is for.
	retry := stub.New()
	retry.Head = "ts-1"
	if err := migrate.RunAdopt(context.Background(), cfg(retry)); err != nil {
		t.Fatalf("a retried adopt after a partial record did not converge: %v", err)
	}
	var recorded bool
	for _, c := range retry.Calls() {
		if c.GetUpSql() == "-- record ts-2" {
			recorded = true
		}
	}
	if !recorded {
		t.Error("the retry finished without recording the migration the first run failed on")
	}
}

// TestRunAdopt_ManagedDatabaseWithRealPendingWorkStillRefuses — the guard
// the change above must not have removed. A database that is genuinely
// under migration management, with a migration that is pending because its
// schema is genuinely not there, has nothing to adopt.
func TestRunAdopt_ManagedDatabaseWithRealPendingWorkStillRefuses(t *testing.T) {
	m := mkAdoptable("ts-2", "main", "CREATE TABLE b;")
	dir := seedDir(t, m)

	db := stub.New()
	db.Head = "ts-0"
	db.FailOn = m.GetId()
	db.FailErr = errors.New("relation \"b\" does not exist")

	err := migrate.RunAdopt(context.Background(), migrate.Config{
		MigrationsDir:    dir,
		Out:              io.Discard,
		Targets:          targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		AdoptConnections: []string{"main"},
		ApplierFor:       func(string) (migrate.Applier, error) { return db, nil },
	})
	if err == nil {
		t.Fatal("adopt recorded a migration whose schema the database does not hold")
	}
	for _, want := range []string{"already under migration management", "migrate apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not point a managed database at its real way forward (missing %q): %v", want, err)
		}
	}
	assertNothingRecorded(t, "main", db.Calls())
}
