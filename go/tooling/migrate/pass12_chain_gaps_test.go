package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// T2-5 pass #12 — the three gaps the chain (B11-1) left open.
//
// B11-1 brought the migration BODIES under a hash the signed lock transitively
// pins. It did not bring everything that decides WHAT EXECUTES under it, and it
// was wired into the forward apply path only. Three lanes of pass #12 converged
// on that, and each of the scenarios below was reproduced by measurement before
// any of this was written:
//
//   - `supersedes` + `adopt_sql` ride outside the hash AND outside the console's
//     ed25519 signature (which covers id/direction/project/connection/up/post).
//     They decide whether a migration's real DDL runs and supply the SQL that
//     runs instead.
//   - `id` rides outside the hash and is the cutoff the pending filter uses.
//   - `PlanRollback` never walks the chain, so the destructive sibling of apply
//     still selects by id range.
//
// The corridor is the one B11-1 measured: CI bakes the fetch output, the deploy
// host applies offline against those files (docs/decisions/deploy-client-architecture.md).

// mutateOnDisk rewrites one migration artifact in place, applying `edit` to the
// decoded message and writing it back WITHOUT recomputing content_sha256 —
// modelling an attacker who edits a field the digest does not cover. If the
// field were covered, the artifact would fail its own integrity check on load
// and none of these tests could reach the behaviour they assert.
func mutateOnDisk(t *testing.T, dir, conn, id string, edit func(*applyfetchpb.Migration)) {
	t.Helper()
	path := filepath.Join(dir, conn, id+".json")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := &applyfetchpb.Migration{}
	if err := protojson.Unmarshal(buf, m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	before := m.GetContentSha256()
	edit(m)
	if m.GetContentSha256() != before {
		t.Fatalf("mutateOnDisk must not touch content_sha256 — the point is that the digest does NOT cover the edited field")
	}
	out, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	// The artifact is keyed by id on disk; an id edit moves the file.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(dir, conn, m.GetId()+".json"), out, 0o644); err != nil {
		t.Fatalf("write %s: %v", m.GetId(), err)
	}
}

// F1 (B12-1 ≡ D12-1, CRITICAL) — tampering `supersedes` + `adopt_sql` on an
// INTERMEDIATE migration turns it into an adopt: the attacker's SQL executes as
// that migration and the real up_sql never runs. Measured before the fix:
// exit 0, applier saw the GRANT, `CREATE TABLE b();` never ran.
func TestRun_TamperedAdoptFieldsAreRefused(t *testing.T) {
	const sentinel = "GRANT ALL ON ALL TABLES IN SCHEMA public TO attacker;"
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)
	pin := lockTarget{"main", m3.GetId(), m3.GetContentSha256()}

	mutateOnDisk(t, dir, "main", "ts-2", func(m *applyfetchpb.Migration) {
		m.Supersedes = []string{"ts-1"}
		m.AdoptSql = sentinel
	})

	stubA := stub.New()
	stubA.Head = "ts-1"
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(pin),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("tampered supersedes/adopt_sql must be refused — they decide whether the real DDL runs and supply what runs instead")
	}
	for _, c := range stubA.Calls() {
		if strings.Contains(c.GetUpSql(), sentinel) {
			t.Errorf("the attacker's SQL executed as %s", c.GetId())
		}
	}
}

// F1, third leg — `supersedes` on its own, with adopt_sql left exactly as the
// console issued it. No attacker SQL runs here; the migration's real DDL is
// simply skipped and a legitimate-looking ledger row is written in its place.
// That is the quieter half of the finding and the one an operator would never
// notice: the deploy succeeds and the schema change is absent.
func TestRun_TamperedSupersedesAloneIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "ALTER TABLE a ADD CONSTRAINT tenant_isolation CHECK (org_id IS NOT NULL);")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)

	mutateOnDisk(t, dir, "main", "ts-2", func(m *applyfetchpb.Migration) {
		m.Supersedes = []string{"ts-1"} // names the applied head → flips to adopt
	})

	stubA := stub.New()
	stubA.Head = "ts-1"
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", m3.GetId(), m3.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("a tampered supersedes must be refused — it alone decides whether a migration's DDL runs")
	}
	for _, c := range stubA.Calls() {
		if strings.Contains(c.GetUpSql(), "tenant_isolation") {
			return // the real DDL ran, which is the correct outcome if anything ran
		}
	}
	if len(stubA.Calls()) > 0 {
		t.Errorf("something executed but not the real constraint migration: %d call(s)", len(stubA.Calls()))
	}
}

// F1, second leg — the residual attack that survives covering `supersedes`
// alone: a GENUINE squash baseline, whose supersedes legitimately names the
// database's head, with only its `adopt_sql` rewritten. Nothing here needs
// forging; the artefact is exactly what the console issued apart from the one
// field that supplies the SQL to run.
//
// Split from the case above deliberately. Tampering both fields at once cannot
// tell which one the digest covers — break-proofing showed the combined test
// stayed green with `adopt_sql` removed from the hash, because `supersedes`
// alone already failed the check.
func TestRun_TamperedAdoptSqlAloneIsRefused(t *testing.T) {
	const sentinel = "GRANT ALL ON ALL TABLES IN SCHEMA public TO attacker;"
	baseline := mkMig("ts-9", "main", "CREATE TABLE everything (id BIGINT PRIMARY KEY);")
	baseline.Supersedes = []string{"ts-1", "ts-2"}
	baseline.AdoptSql = "INSERT INTO wc_migrations VALUES ('ts-9');"
	dir := seedDir(t, baseline)

	mutateOnDisk(t, dir, "main", "ts-9", func(m *applyfetchpb.Migration) { m.AdoptSql = sentinel })

	stubA := stub.New()
	stubA.Head = "ts-2" // this database applied the collapsed range → adopt path
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", baseline.GetId(), baseline.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("a rewritten adopt_sql must be refused — it is the SQL the adopt path executes verbatim")
	}
	for _, c := range stubA.Calls() {
		if strings.Contains(c.GetUpSql(), sentinel) || strings.Contains(c.GetAdoptSql(), sentinel) {
			t.Errorf("the attacker's adopt_sql executed as %s", c.GetId())
		}
	}
}

// F3 (B12-2 ≡ D12-3, HIGH) — `id` is the cutoff the pending filter uses, so
// relabelling an intermediate below the applied head drops its DDL while the
// deploy reports success. Note this evades the missing-predecessor refusal that
// DELETING the file would trip: the body, hash and prev stay intact, so the
// chain walk still reaches it — the id cutoff then silently discards it.
func TestPlan_RelabelledIdIsRefused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "ALTER TABLE a ADD CONSTRAINT tenant_isolation CHECK (org_id IS NOT NULL);")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)
	pin := lockTarget{"main", m3.GetId(), m3.GetContentSha256()}

	mutateOnDisk(t, dir, "main", "ts-2", func(m *applyfetchpb.Migration) { m.Id = "ts-0" })

	stubA := stub.New()
	stubA.Head = "ts-1"
	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(pin),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		var ids []string
		for _, p := range pending {
			ids = append(ids, p.Migration.GetId())
		}
		t.Fatalf("a relabelled id must be refused; Plan returned %v — the constraint migration vanished while the deploy reports success", ids)
	}
}

// F2 (A12-1 ≡ C12-1 ≡ D12-2, HIGH) — the destructive sibling. Apply refuses an
// inserted off-chain migration (TestPlan_InsertedMigrationIsNotOnTheChain);
// rollback ran its down_sql. Same file, same directory, opposite verdict.
func TestRunRollback_OffChainMigrationIsNotRolledBack(t *testing.T) {
	const sentinel = "GRANT ALL ON ALL TABLES IN SCHEMA public TO attacker;"
	m1 := mkMig("ts-1", "main", "CREATE TABLE a();")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b();")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c();")
	dir := seedDir(t, m1, m2, m3)

	// Self-consistent, sorts inside the applied range, never applied, and on no
	// chain: prev is empty, so nothing links to it.
	inserted := &applyfetchpb.Migration{
		Id: "ts-25", Connection: "main",
		UpSql:   "SELECT 1;",
		DownSql: sentinel,
	}
	selfConsistent(inserted)
	if err := migrate.WriteMigration(dir, inserted); err != nil {
		t.Fatalf("seed the inserted migration: %v", err)
	}

	stubA := stub.New()
	stubA.Head = "ts-3"
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets:       targets(lockTarget{"main", m3.GetId(), m3.GetContentSha256()}),
		MigrationsDir: dir,
		ToMigrationID: "ts-1",
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	for _, c := range stubA.RollbackCalls() {
		if c.GetId() == "ts-25" || strings.Contains(c.GetDownSql(), sentinel) {
			t.Errorf("rollback executed the off-chain migration %s — apply refuses this exact file", c.GetId())
		}
	}
	if err == nil {
		t.Error("an off-chain artifact in the rollback range must be refused, not silently skipped: it would leave the operator believing a rollback was complete")
	}
}

// selfConsistent stamps the artifact with the production content hash, so the
// file is valid under whatever rules are currently in force. Written as one
// helper because the hash's inputs are exactly what this pass changes: a test
// that hard-codes them measures yesterday's contract.
func selfConsistent(m *applyfetchpb.Migration) {
	m.ContentSha256 = migrate.ContentHash(
		m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(),
		m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql())
}
