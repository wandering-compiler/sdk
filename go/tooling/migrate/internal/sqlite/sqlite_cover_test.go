package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// newTempApplier opens an Applier against a fresh file DB in a temp
// dir and registers cleanup. Centralises the boilerplate so each
// behaviour test reads as one concern.
func newTempApplier(t *testing.T) *sqlite.Applier {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestParseDSN_Empty pins that an empty DSN is rejected at parse
// time rather than deferred to the driver.
func TestParseDSN_Empty(t *testing.T) {
	_, err := sqlite.ParseDSN("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestParseDSN_NormalisesURLForm pins that ParseDSN carries the raw
// input through URLDSN and exposes the driver-native form in
// DriverDSN (URL prefix stripped).
func TestParseDSN_NormalisesURLForm(t *testing.T) {
	cfg, err := sqlite.ParseDSN("sqlite:///abs/path.db")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if cfg.URLDSN != "sqlite:///abs/path.db" {
		t.Errorf("URLDSN = %q, want raw input", cfg.URLDSN)
	}
	if cfg.DriverDSN != "/abs/path.db" {
		t.Errorf("DriverDSN = %q, want /abs/path.db", cfg.DriverDSN)
	}
}

// TestValidate_RejectsEmptyDriverDSN pins that Validate fails when
// the normalised driver DSN is empty (the only invariant it holds).
func TestValidate_RejectsEmptyDriverDSN(t *testing.T) {
	if err := sqlite.Validate(sqlite.Config{}); err == nil {
		t.Error("expected empty-DSN error on zero Config")
	}
}

// TestValidate_AcceptsParsed pins that a Config produced by ParseDSN
// always passes Validate (parse + validate compose cleanly).
func TestValidate_AcceptsParsed(t *testing.T) {
	cfg, err := sqlite.ParseDSN("file:rel.db?cache=shared")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if err := sqlite.Validate(cfg); err != nil {
		t.Errorf("Validate of parsed config: %v", err)
	}
}

// TestNew_PingFailsOnUnwritablePath pins that a path whose parent
// directory does not exist surfaces at New (the eager ping), not
// silently on first Apply.
func TestNew_PingFailsOnUnwritablePath(t *testing.T) {
	_, err := sqlite.New(context.Background(),
		"sqlite:///no/such/dir/does/not/exist.db")
	if err == nil || !strings.Contains(err.Error(), "ping") {
		t.Errorf("expected ping error on missing directory, got %v", err)
	}
}

// TestAppliedHead_FreshDBEmpty pins the missing-table → "" contract:
// a brand-new DB has no wc_migrations table, which the orchestrator
// reads as a fresh DB rather than an error.
func TestAppliedHead_FreshDBEmpty(t *testing.T) {
	a := newTempApplier(t)
	head, err := a.AppliedHead(context.Background())
	if err != nil {
		t.Fatalf("AppliedHead on fresh DB: %v", err)
	}
	if head != "" {
		t.Errorf("fresh DB head = %q, want empty string", head)
	}
}

// TestAppliedHead_ReturnsMaxTimestamp pins that AppliedHead returns
// the lexically-greatest timestamp once wc_migrations is populated.
func TestAppliedHead_ReturnsMaxTimestamp(t *testing.T) {
	a := newTempApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "ts-1",
		UpSql: "BEGIN;" +
			"CREATE TABLE wc_migrations (timestamp TEXT PRIMARY KEY);" +
			"INSERT INTO wc_migrations VALUES ('20240101T000000Z');" +
			"INSERT INTO wc_migrations VALUES ('20240202T000000Z');" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}
	head, err := a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead: %v", err)
	}
	if head != "20240202T000000Z" {
		t.Errorf("head = %q, want 20240202T000000Z", head)
	}
}

// TestAppliedHead_NonMissingTableErrorWraps pins that an error which
// is NOT a missing-table condition (here: wc_migrations exists but
// lacks the timestamp column) propagates wrapped, not swallowed.
func TestAppliedHead_NonMissingTableErrorWraps(t *testing.T) {
	a := newTempApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "CREATE TABLE wc_migrations (other TEXT);",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}
	_, err := a.AppliedHead(ctx)
	if err == nil || !strings.Contains(err.Error(), "sqlite AppliedHead") {
		t.Errorf("expected wrapped AppliedHead error, got %v", err)
	}
}

// TestRollback_HappyPath pins that Rollback runs down_pre_tx then
// down_sql end-to-end against a real DB, dropping what Apply built.
func TestRollback_HappyPath(t *testing.T) {
	a := newTempApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "BEGIN; CREATE TABLE t (id INTEGER); CREATE INDEX t_id ON t(id); COMMIT;",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := a.Rollback(ctx, &applyfetchpb.Migration{
		Id:        "ts-1",
		DownPreTx: "DROP INDEX t_id;",
		DownSql:   "BEGIN; DROP TABLE t; COMMIT;",
	}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Table is gone — re-applying the same CREATE must succeed.
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ts-2",
		UpSql: "CREATE TABLE t (id INTEGER);",
	}); err != nil {
		t.Fatalf("re-Apply after rollback: %v", err)
	}
}

// TestRollback_EmptyPayloadNoop pins that a Migration with neither
// down_pre_tx nor down_sql is a clean no-op (both branches skipped).
func TestRollback_EmptyPayloadNoop(t *testing.T) {
	a := newTempApplier(t)
	if err := a.Rollback(context.Background(), &applyfetchpb.Migration{Id: "empty"}); err != nil {
		t.Errorf("empty Rollback: %v", err)
	}
}

// TestRollback_BadPreTxPropagates pins that a malformed down_pre_tx
// surfaces with the down_pre_tx phase prefix.
func TestRollback_BadPreTxPropagates(t *testing.T) {
	a := newTempApplier(t)
	err := a.Rollback(context.Background(), &applyfetchpb.Migration{
		Id:        "bad",
		DownPreTx: "NOT VALID SQL;",
	})
	if err == nil || !strings.Contains(err.Error(), "down_pre_tx") {
		t.Errorf("expected down_pre_tx error, got %v", err)
	}
}

// TestRollback_BadDownSqlPropagates pins that a malformed down_sql
// surfaces with the down_sql phase prefix.
func TestRollback_BadDownSqlPropagates(t *testing.T) {
	a := newTempApplier(t)
	err := a.Rollback(context.Background(), &applyfetchpb.Migration{
		Id:      "bad",
		DownSql: "DROP TABLE does_not_exist;",
	})
	if err == nil || !strings.Contains(err.Error(), "down_sql") {
		t.Errorf("expected down_sql error, got %v", err)
	}
}

// TestApply_CancelledContextFailsToAcquireConn pins that an already
// cancelled context surfaces at conn acquisition — Apply must not
// proceed to run DDL on a dead context.
func TestApply_CancelledContextFailsToAcquireConn(t *testing.T) {
	a := newTempApplier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "x", UpSql: "SELECT 1;"})
	if err == nil || !strings.Contains(err.Error(), "sqlite apply") {
		t.Errorf("expected apply error on cancelled ctx, got %v", err)
	}
}

// TestRollback_CancelledContextFailsToAcquireConn — Rollback's
// conn-acquire guard mirrors Apply's; a cancelled context must abort
// before any down payload runs.
func TestRollback_CancelledContextFailsToAcquireConn(t *testing.T) {
	a := newTempApplier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Rollback(ctx, &applyfetchpb.Migration{Id: "x", DownSql: "SELECT 1;"})
	if err == nil || !strings.Contains(err.Error(), "sqlite rollback") {
		t.Errorf("expected rollback error on cancelled ctx, got %v", err)
	}
}

// TestFingerprint_ExtractErrorPropagates pins that a failure inside
// schema extraction (here forced via a cancelled context) propagates
// out of Fingerprint rather than yielding a bogus empty hash.
func TestFingerprint_ExtractErrorPropagates(t *testing.T) {
	a := newTempApplier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Fingerprint(ctx)
	if err == nil {
		t.Error("expected extract error on cancelled ctx")
	}
}

// TestFingerprint_StableAndSchemaSensitive pins two things: the
// fingerprint is deterministic for an unchanged schema, and it
// changes when the schema changes (so drift detection can fire).
func TestFingerprint_StableAndSchemaSensitive(t *testing.T) {
	a := newTempApplier(t)
	ctx := context.Background()

	fpEmpty, err := a.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("Fingerprint (empty): %v", err)
	}

	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fpAfter, err := a.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("Fingerprint (after): %v", err)
	}
	if fpAfter == "" {
		t.Fatal("fingerprint is empty string")
	}
	if fpAfter == fpEmpty {
		t.Error("fingerprint unchanged after adding a table")
	}

	fpAgain, err := a.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("Fingerprint (again): %v", err)
	}
	if fpAgain != fpAfter {
		t.Errorf("fingerprint not deterministic: %q != %q", fpAgain, fpAfter)
	}
}
