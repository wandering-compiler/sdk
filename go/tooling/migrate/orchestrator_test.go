package migrate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// mkMig builds a plain Migration whose content_sha256 matches its
// (un-decorated) up body — the on-the-wire shape the production
// console now stores (migrations are fetched pre-verified; the
// client holds no verifier key). loadConnectionMigrations only
// checks sha256(up_sql) == content_sha256.
func mkMig(id, conn, body string) *applyfetchpb.Migration {
	return mkMigFull(id, conn, body, "", "", "-- down for "+id)
}

// mkMigFull is mkMig with explicit up_post_tx / down_pre_tx /
// down_sql so tests can exercise the non-transactional skirt
// (CREATE/DROP INDEX CONCURRENTLY) and the down-body path.
// content_sha256 is the canonical hash over all four segments.
func mkMigFull(id, conn, up, upPostTx, downPreTx, downSql string) *applyfetchpb.Migration {
	m := &applyfetchpb.Migration{
		Id:         id,
		Connection: conn,
		UpSql:      up,
		UpPostTx:   upPostTx,
	}
	if downSql != "" || downPreTx != "" {
		m.DownPreTx = downPreTx
		m.DownSql = downSql
	}
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql())
	return m
}

// seedDir writes every migration to a temp directory using the
// canonical fetch-side layout (migrate.WriteMigration). Returns the
// root directory.
func seedDir(t *testing.T, migs ...*applyfetchpb.Migration) string {
	t.Helper()
	dir := t.TempDir()
	for _, m := range migs {
		if err := migrate.WriteMigration(dir, m); err != nil {
			t.Fatalf("WriteMigration %s: %v", m.GetId(), err)
		}
	}
	return dir
}

// targets builds the per-connection apply ceilings the orchestrator
// reads (replacing the old lock proto). Caller names the target id +
// matching content hash per connection.
func targets(ts ...lockTarget) []migrate.ConnTarget {
	out := make([]migrate.ConnTarget, 0, len(ts))
	for _, t := range ts {
		out = append(out, migrate.ConnTarget{Connection: t.connection, TargetMigrationID: t.id, TargetContentSha256: t.hash})
	}
	return out
}

type lockTarget struct {
	connection string
	id         string
	hash       string
}

// TestPlan_HappyPath — fresh DB (Head=""), all artifacts on
// disk, target pinned at last id → every migration is pending.
func TestPlan_HappyPath(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE x;")
	m2 := mkMig("ts-2", "main", "ALTER TABLE x ADD col;")
	dir := seedDir(t, m1, m2)
	tg := targets(
		lockTarget{"main", m2.GetId(), m2.GetContentSha256()},
	)
	stubA := stub.New() // Head="" by default

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len = %d, want 2", len(pending))
	}
	for i, want := range []string{"ts-1", "ts-2"} {
		if pending[i].Migration.GetId() != want {
			t.Errorf("[%d] id = %q, want %q", i, pending[i].Migration.GetId(), want)
		}
	}
}

// TestPlan_PartiallyApplied — Head=ts-1 means ts-1 already on
// the DB; only ts-2 should be pending.
func TestPlan_PartiallyApplied(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE x;")
	m2 := mkMig("ts-2", "main", "ALTER TABLE x ADD col;")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})

	stubA := stub.New()
	stubA.Head = "ts-1"

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 1 || pending[0].Migration.GetId() != "ts-2" {
		t.Errorf("got %+v, want [ts-2]", pending)
	}
}

// TestPlan_TargetMidHistory — target_migration_id pins to a
// middle migration; later migrations on disk are NOT included.
func TestPlan_TargetMidHistory(t *testing.T) {
	m1 := mkMig("ts-1", "main", "x")
	m2 := mkMig("ts-2", "main", "y")
	m3 := mkMig("ts-3", "main", "z")
	dir := seedDir(t, m1, m2, m3)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len = %d, want 2 (ts-1 + ts-2; ts-3 is past target)", len(pending))
	}
	if pending[1].Migration.GetId() != "ts-2" {
		t.Errorf("last pending = %q, want ts-2 (target)", pending[1].Migration.GetId())
	}
}

// TestPlan_TargetNotOnDisk — target pins X but artifact for X
// never landed on disk → loud refusal asking the operator to run
// fetch.
func TestPlan_TargetNotOnDisk(t *testing.T) {
	m1 := mkMig("ts-1", "main", "x")
	dir := seedDir(t, m1)
	tg := targets(lockTarget{"main", "ts-99-missing", "0000"})

	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ts-99-missing") {
		t.Errorf("expected target-not-found error, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "migrate fetch") {
		t.Errorf("error should hint at fetch; got %v", err)
	}
}

// TestPlan_TargetHashMismatch — disk artifact + target pin to the
// same id but the pinned hash doesn't match the artifact's hash →
// refusal (someone hand-edited).
func TestPlan_TargetHashMismatch(t *testing.T) {
	m := mkMig("ts-1", "main", "real body")
	dir := seedDir(t, m)
	wrongHash := strings.Repeat("0", 64)
	tg := targets(lockTarget{"main", "ts-1", wrongHash})

	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "target_content_sha256") {
		t.Errorf("expected hash mismatch error, got %v", err)
	}
}

// TestPlan_NoTargetSkipsConnection — connection without a target
// id is ignored (operator hasn't pushed).
func TestPlan_NoTargetSkipsConnection(t *testing.T) {
	dir := seedDir(t)                                // empty
	tg := []migrate.ConnTarget{{Connection: "main"}} // no target
	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("connection without target should yield no pending; got %d", len(pending))
	}
}

// TestPlan_OrderedAcrossConnections — D41 lex name order across
// connections.
func TestPlan_OrderedAcrossConnections(t *testing.T) {
	a1 := mkMig("a-1", "alpha", "alpha 1")
	b1 := mkMig("b-1", "beta", "beta 1")
	dir := seedDir(t, a1, b1)
	tg := targets(
		lockTarget{"beta", b1.GetId(), b1.GetContentSha256()},
		lockTarget{"alpha", a1.GetId(), a1.GetContentSha256()},
	)

	pending, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len = %d, want 2", len(pending))
	}
	if pending[0].Connection != "alpha" || pending[1].Connection != "beta" {
		t.Errorf("expected alpha before beta (lex order); got %s, %s",
			pending[0].Connection, pending[1].Connection)
	}
}

// TestPlan_EmptyTargetsYieldsEmpty — no targets pinned is a valid
// no-op (empty result), not an error.
func TestPlan_EmptyTargetsYieldsEmpty(t *testing.T) {
	pending, err := migrate.Plan(context.Background(), migrate.Config{
		MigrationsDir: t.TempDir(),
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("empty targets should yield no pending; got %d", len(pending))
	}
}

func TestPlan_EmptyMigrationsDirErrors(t *testing.T) {
	tg := targets(lockTarget{"main", "x", "y"})
	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:    tg,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "MigrationsDir is empty") {
		t.Errorf("expected MigrationsDir error, got %v", err)
	}
}

func TestPlan_NilApplierForErrors(t *testing.T) {
	_, err := migrate.Plan(context.Background(), migrate.Config{
		MigrationsDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "ApplierFor is nil") {
		t.Errorf("expected ApplierFor-nil error, got %v", err)
	}
}

// TestPlan_AppliedHeadError — driver-side AppliedHead failure
// surfaces verbatim wrapped with the connection name.
func TestPlan_AppliedHeadError(t *testing.T) {
	m := mkMig("ts-1", "main", "x")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})

	stubA := stub.New()
	stubA.HeadErr = errors.New("connection refused")

	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected AppliedHead error, got %v", err)
	}
}

// TestRun_DryRunPrintsSQL — dry-run prints pending SQL + does
// NOT call Applier.Apply.
func TestRun_DryRunPrintsSQL(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE users (id BIGINT);")
	m2 := mkMig("ts-2", "main", "ALTER TABLE users ADD email TEXT;")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})
	stubA := stub.New()
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, DryRun: true, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"dry-run — 2 migration(s) would apply",
		"main :: ts-1",
		"CREATE TABLE users",
		"ALTER TABLE users ADD email TEXT",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q. got:\n%s", want, body)
		}
	}
	if len(stubA.Calls()) != 0 {
		t.Error("dry-run should not call Applier.Apply")
	}
}

// TestRun_HappyPath — full apply loop walks every pending in
// order and calls Applier.Apply.
func TestRun_HappyPath(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE x;")
	m2 := mkMig("ts-2", "main", "ALTER TABLE x ADD col;")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})
	stubA := stub.New()
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := stubA.Calls()
	if len(calls) != 2 || calls[0].GetId() != "ts-1" || calls[1].GetId() != "ts-2" {
		t.Errorf("applier call sequence wrong: %+v", calls)
	}
}

// TestRun_NothingPending — empty pending list short-circuits
// with a "nothing pending" message.
func TestRun_NothingPending(t *testing.T) {
	m := mkMig("ts-1", "main", "x")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})

	stubA := stub.New()
	stubA.Head = "ts-1" // already applied
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "nothing pending") {
		t.Errorf("output should announce no work; got:\n%s", out.String())
	}
	if len(stubA.Calls()) != 0 {
		t.Error("applier should not be called when nothing is pending")
	}
}

// TestRun_ApplierError — failed Apply aborts mid-list.
func TestRun_ApplierError(t *testing.T) {
	m1 := mkMig("ts-1", "main", "OK first")
	m2 := mkMig("ts-2", "main", "FAIL HERE")
	m3 := mkMig("ts-3", "main", "skipped")
	dir := seedDir(t, m1, m2, m3)
	tg := targets(lockTarget{"main", m3.GetId(), m3.GetContentSha256()})

	stubA := stub.New()
	stubA.FailOn = "ts-2"
	stubA.FailErr = errors.New("dialect boom")

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ts-2") {
		t.Fatalf("expected ts-2 failure, got %v", err)
	}
	if got := len(stubA.Calls()); got != 2 {
		t.Errorf("applier saw %d calls, want 2 (ts-1 + ts-2; ts-3 must not be attempted)", got)
	}
}

// TestRun_OutNilUsesDiscard — nil Out doesn't panic.
func TestRun_OutNilUsesDiscard(t *testing.T) {
	dir := t.TempDir()
	err := migrate.Run(context.Background(), migrate.Config{
		MigrationsDir: dir, DryRun: true,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Errorf("Run with nil Out: %v", err)
	}
}

// TestPlan_DiskTamperedRefuses — operator (or attacker) edited
// an .up.sql file but not the .json's content_sha256 → load
// step refuses.
func TestPlan_DiskTamperedRefuses(t *testing.T) {
	m := mkMig("ts-1", "main", "real")
	dir := seedDir(t, m)
	// Manually rewrite the .json with a different up_sql but the
	// SAME content_sha256 — i.e. someone tampered with the body
	// to slip in malicious SQL while leaving the registry-side
	// hash intact.
	jsonPath := filepath.Join(dir, "main", "ts-1.json")
	good, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	tampered := strings.Replace(string(good), `"up_sql": "real"`, `"up_sql": "EVIL"`, 1)
	if tampered == string(good) {
		t.Skip("protojson key spelling shifted — fix test harness")
	}
	if err := os.WriteFile(jsonPath, []byte(tampered), 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})
	_, err = migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Errorf("expected content_sha256 mismatch error, got %v", err)
	}
}

// TestWriteMigration_HashMismatchRefuses — fetch-side
// integrity guard: WriteMigration recomputes hash and refuses
// before any disk write.
func TestWriteMigration_HashMismatchRefuses(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id: "ts-1", Connection: "main",
		UpSql:         "real body",
		ContentSha256: strings.Repeat("0", 64), // wrong
	}
	err := migrate.WriteMigration(t.TempDir(), m)
	if err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Errorf("expected hash mismatch error, got %v", err)
	}
}

func TestWriteMigration_RequiresFields(t *testing.T) {
	for i, m := range []*applyfetchpb.Migration{
		nil,
		{},
		{Id: "x"}, // no connection
	} {
		err := migrate.WriteMigration(t.TempDir(), m)
		if err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

// ─── Rollback tests ─────────────────────────────────────────

// TestPlanRollback_Reverse — applied head at ts-3, target = ts-1
// → rollback ts-3 then ts-2 (in reverse).
func TestPlanRollback_Reverse(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	m2 := mkMig("ts-2", "main", "v1")
	m3 := mkMig("ts-3", "main", "v2")
	dir := seedDir(t, m1, m2, m3)
	tg := targets(lockTarget{"main", m3.GetId(), m3.GetContentSha256()})
	stubA := stub.New()
	stubA.Head = "ts-3"

	pending, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(pending) != 2 ||
		pending[0].Migration.GetId() != "ts-3" ||
		pending[1].Migration.GetId() != "ts-2" {
		t.Errorf("expected rollback order [ts-3, ts-2]; got %+v", pending)
	}
}

// TestPlanRollback_FreshDB — empty AppliedHead = nothing to roll
// back regardless of disk artifacts.
func TestPlanRollback_FreshDB(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	dir := seedDir(t, m1)
	tg := targets(lockTarget{"main", m1.GetId(), m1.GetContentSha256()})
	stubA := stub.New() // Head=""

	pending, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("fresh DB should have nothing to roll back; got %d", len(pending))
	}
}

// TestRunRollback_HappyPath — RunRollback walks pending in
// reverse order, calls Applier.Rollback for each.
func TestRunRollback_HappyPath(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	m2 := mkMig("ts-2", "main", "v1")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})
	stubA := stub.New()
	stubA.Head = "ts-2"
	var out bytes.Buffer

	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1", Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	rolls := stubA.RollbackCalls()
	if len(rolls) != 1 || rolls[0].GetId() != "ts-2" {
		t.Errorf("expected single rollback of ts-2, got %+v", rolls)
	}
	if !strings.Contains(out.String(), "1 migration(s) rolled back") {
		t.Errorf("output missing summary line; got:\n%s", out.String())
	}
}

// TestRunRollback_DryRun — DryRun stops after Plan + prints
// down_sql for each pending; never calls Applier.Rollback.
func TestRunRollback_DryRun(t *testing.T) {
	m := mkMig("ts-1", "main", "real")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})
	stubA := stub.New()
	stubA.Head = "ts-1"
	var out bytes.Buffer

	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "", DryRun: true, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("RunRollback dry-run: %v", err)
	}
	if len(stubA.RollbackCalls()) != 0 {
		t.Error("dry-run should not call Applier.Rollback")
	}
	if !strings.Contains(out.String(), "dry-run — 1 migration(s)") {
		t.Errorf("dry-run output missing dry-run notice; got:\n%s", out.String())
	}
}

// TestRunRollback_NothingPending — head ≤ ToMigrationID = nothing
// to roll back.
func TestRunRollback_NothingPending(t *testing.T) {
	m := mkMig("ts-1", "main", "v0")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})
	stubA := stub.New()
	stubA.Head = "ts-1"

	var out bytes.Buffer
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1", Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to roll back") {
		t.Errorf("expected 'nothing to roll back' notice; got:\n%s", out.String())
	}
}

// TestRun_DryRunPrintsPostTx — apply --dry-run must surface the
// up_post_tx (non-transactional skirt) DDL the real apply will
// run, not just up_sql. A CREATE INDEX CONCURRENTLY lands in
// up_post_tx; an operator auditing via dry-run has to see it.
func TestRun_DryRunPrintsPostTx(t *testing.T) {
	m := mkMigFull("ts-1", "main",
		"BEGIN;\nCREATE TABLE x (id BIGINT);\nCOMMIT;",
		"CREATE INDEX CONCURRENTLY idx_x ON x(id);",
		"", "")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, DryRun: true, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "CREATE INDEX CONCURRENTLY idx_x") {
		t.Errorf("dry-run must print up_post_tx DDL; got:\n%s", out.String())
	}
}

// TestRunRollback_DryRunPrintsPreTx — rollback --dry-run must
// surface down_pre_tx (the symmetric non-transactional skirt),
// e.g. a DROP INDEX CONCURRENTLY whose down body lives entirely
// in down_pre_tx with an empty down_sql.
func TestRunRollback_DryRunPrintsPreTx(t *testing.T) {
	m := mkMigFull("ts-1", "main", "v0", "",
		"DROP INDEX CONCURRENTLY idx_x;", "")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", m.GetId(), m.GetContentSha256()})
	stubA := stub.New()
	stubA.Head = "ts-1"
	var out bytes.Buffer

	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "", DryRun: true, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("RunRollback dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "DROP INDEX CONCURRENTLY idx_x") {
		t.Errorf("rollback dry-run must print down_pre_tx DDL; got:\n%s", out.String())
	}
}

// TestRun_LogFormatJSON — every applied migration emits one
// JSON line on cfg.Out with the expected structured shape.
func TestRun_LogFormatJSON(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	m2 := mkMig("ts-2", "main", "v1")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})
	stubA := stub.New()
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, Out: &out, LogFormat: "json",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Walk stdout, find lines that parse as JSON, assert each
	// carries action/connection/migration_id/status.
	jsonLines := 0
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec struct {
			Action      string `json:"action"`
			Connection  string `json:"connection"`
			MigrationID string `json:"migration_id"`
			Status      string `json:"status"`
			DurationMs  int64  `json:"duration_ms"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("bad json line %q: %v", line, err)
			continue
		}
		if rec.Action != "apply" {
			t.Errorf("expected action=apply, got %q in %q", rec.Action, line)
		}
		if rec.Connection != "main" {
			t.Errorf("expected connection=main, got %q", rec.Connection)
		}
		if rec.Status != "ok" {
			t.Errorf("expected status=ok, got %q", rec.Status)
		}
		if rec.MigrationID == "" {
			t.Errorf("missing migration_id in %q", line)
		}
		jsonLines++
	}
	if jsonLines != 2 {
		t.Errorf("expected 2 JSON log lines (one per applied migration), got %d", jsonLines)
	}
}

// TestPlanRollback_SkipsUnpinnedConnection (ROLLBACK-UNPINNED) — a connection
// with no pinned target (never pushed) is skipped without opening an applier, so
// an ApplierFor that can't resolve it doesn't abort the whole rollback.
func TestPlanRollback_SkipsUnpinnedConnection(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a;")
	dir := seedDir(t, m1)
	stubA := stub.New()
	stubA.Head = "ts-1"
	tg := targets(
		lockTarget{"main", m1.GetId(), m1.GetContentSha256()},
		lockTarget{"secondary", "", ""}, // declared but never pushed
	)
	pending, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(conn string) (migrate.Applier, error) {
			if conn == "secondary" {
				return nil, errors.New("no --target configured for connection \"secondary\"")
			}
			return stubA, nil
		},
	})
	if err != nil {
		t.Fatalf("ROLLBACK-UNPINNED: an unpinned connection must be skipped, got %v", err)
	}
	if len(pending) != 1 || pending[0].Migration.GetId() != "ts-1" {
		t.Fatalf("expected only main/ts-1 to roll back, got %+v", pending)
	}
}

// TestPlanRollback_RefusesTamperedTargetPin (ROLLBACK-NO-PIN) — a lock pin that
// disagrees with the on-disk artifact refuses the (destructive) rollback.
func TestPlanRollback_RefusesTamperedTargetPin(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a;")
	dir := seedDir(t, m1)
	stubA := stub.New()
	stubA.Head = "ts-1"
	tg := targets(lockTarget{"main", m1.GetId(), "deadbeefmismatch"}) // pin != artifact hash
	_, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "refusing rollback") {
		t.Fatalf("ROLLBACK-NO-PIN: a tampered target pin must refuse rollback, got %v", err)
	}
}
