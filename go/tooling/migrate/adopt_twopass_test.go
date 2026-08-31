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

// mkAdoptable is mkMig plus the two statements an adoption needs: the
// preflight that asks whether the database already holds what the migration
// describes, and the record body that writes the ledger row without running
// any DDL.
func mkAdoptable(id, conn, body string) *applyfetchpb.Migration {
	m := mkMig(id, conn, body)
	m.AdoptPreflightSql = "-- preflight for " + id
	m.AdoptSql = "-- record " + id
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(),
		m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), "")
	return m
}

// TestRunAdopt_RefusesEveryConnectionBeforeRecordingAny — an adoption that
// cannot finish must not have started.
//
// Hosted review 2026-08-30, HOST-D-1. `c4e9e07eb` hoisted the ARTIFACT-shape
// check above the per-connection loop and its comment claimed the whole rule
// — "verify everything, then record everything — lifted one level". Two of
// the three refusal causes were still inside the loop: a connection whose
// ledger is non-empty, and a connection whose preflight the database does
// not satisfy. So connection A was fully recorded, and only then did B
// refuse — the exact half-adopted state the inner two-pass split exists to
// prevent, one level up.
//
// `RunAdopt` had no unit tests at all, which is how a commit could certify
// an invariant it did not deliver.
func TestRunAdopt_RefusesEveryConnectionBeforeRecordingAny(t *testing.T) {
	a := mkAdoptable("ts-1", "alpha", "CREATE TABLE a;")
	b := mkAdoptable("ts-2", "beta", "CREATE TABLE b;")
	dir := seedDir(t, a, b)

	t.Run("a later connection's non-empty ledger", func(t *testing.T) {
		alpha, beta := stub.New(), stub.New()
		// beta is already under migration management, with ts-2 still
		// pending — the refusal that used to fire after alpha had been
		// recorded.
		//
		// The head must sort BEFORE the pending migration or Plan's own
		// cutoff drops the connection and adopt never looks at it. That
		// cutoff is exactly why a retry converges, and why this is a
		// transient window rather than a wedge.
		beta.Head = "ts-0"

		err := migrate.RunAdopt(context.Background(), migrate.Config{
			MigrationsDir: dir,
			Out:           io.Discard,
			Targets: targets(
				lockTarget{"alpha", a.GetId(), a.GetContentSha256()},
				lockTarget{"beta", b.GetId(), b.GetContentSha256()},
			),
			AdoptConnections: []string{"alpha", "beta"},
			ApplierFor: func(conn string) (migrate.Applier, error) {
				if conn == "beta" {
					return beta, nil
				}
				return alpha, nil
			},
		})
		if err == nil {
			t.Fatal("adopt succeeded although beta is already managed")
		}
		if !strings.Contains(err.Error(), "before writing anything") {
			t.Errorf("the refusal should say nothing was written; got: %v", err)
		}
		assertNothingRecorded(t, "alpha", alpha.Calls())
	})

	t.Run("a later connection's failing preflight", func(t *testing.T) {
		alpha, beta := stub.New(), stub.New()
		// beta's database does not hold what its migration describes.
		beta.FailOn = b.GetId()
		beta.FailErr = errors.New("relation \"b\" does not exist")

		err := migrate.RunAdopt(context.Background(), migrate.Config{
			MigrationsDir: dir,
			Out:           io.Discard,
			Targets: targets(
				lockTarget{"alpha", a.GetId(), a.GetContentSha256()},
				lockTarget{"beta", b.GetId(), b.GetContentSha256()},
			),
			AdoptConnections: []string{"alpha", "beta"},
			ApplierFor: func(conn string) (migrate.Applier, error) {
				if conn == "beta" {
					return beta, nil
				}
				return alpha, nil
			},
		})
		if err == nil {
			t.Fatal("adopt succeeded although beta's preflight fails")
		}
		assertNothingRecorded(t, "alpha", alpha.Calls())
	})
}

// TestRunAdopt_RecordsEveryConnectionWhenAllChecksPass — the other
// direction. A two-pass split that refused everything would pass both
// assertions above and be useless.
func TestRunAdopt_RecordsEveryConnectionWhenAllChecksPass(t *testing.T) {
	a := mkAdoptable("ts-1", "alpha", "CREATE TABLE a;")
	b := mkAdoptable("ts-2", "beta", "CREATE TABLE b;")
	dir := seedDir(t, a, b)

	alpha, beta := stub.New(), stub.New()
	err := migrate.RunAdopt(context.Background(), migrate.Config{
		MigrationsDir: dir,
		Out:           io.Discard,
		Targets: targets(
			lockTarget{"alpha", a.GetId(), a.GetContentSha256()},
			lockTarget{"beta", b.GetId(), b.GetContentSha256()},
		),
		AdoptConnections: []string{"alpha", "beta"},
		ApplierFor: func(conn string) (migrate.Applier, error) {
			if conn == "beta" {
				return beta, nil
			}
			return alpha, nil
		},
	})
	if err != nil {
		t.Fatalf("adopt refused although both connections check out: %v", err)
	}
	for name, s := range map[string]*stub.Applier{"alpha": alpha, "beta": beta} {
		var recorded bool
		for _, c := range s.Calls() {
			if strings.HasPrefix(c.GetUpSql(), "-- record ") {
				recorded = true
			}
		}
		if !recorded {
			t.Errorf("%s was never recorded — adopt did nothing on a connection that passed every check", name)
		}
	}
}

// assertNothingRecorded fails when a connection took a RECORD write. A
// preflight is allowed: it is the check itself, and its ledger-table CREATE
// is the one write the design permits.
func assertNothingRecorded(t *testing.T, conn string, calls []*applyfetchpb.Migration) {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c.GetUpSql(), "-- record ") {
			t.Errorf("%s was recorded before a later connection refused — the project is left neither adopted nor clean, and no later command knows it happened", conn)
		}
	}
}
