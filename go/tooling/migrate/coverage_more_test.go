package migrate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// ─── white-box fakes + helpers ──────────────────────────────────
//
// White-box tests (package apply) cannot import the apply/stub
// package — stub imports apply, which is an import cycle in an
// internal test binary. So this file carries its own configurable
// Applier fake covering every error knob the orchestrator can hit:
// head/headErr (plan cutoff), closeErr (close-after-head), failOn/
// failErr (mid-list apply/rollback failure).

type covFake struct {
	head     string
	headErr  error
	closeErr error
	failOn   string
	failErr  error
	applied  []string
	rolled   []string
}

func (f *covFake) Apply(_ context.Context, m *applyfetchpb.Migration) error {
	f.applied = append(f.applied, m.GetId())
	if f.failOn != "" && m.GetId() == f.failOn {
		return f.failErr
	}
	return nil
}

func (f *covFake) Rollback(_ context.Context, m *applyfetchpb.Migration) error {
	f.rolled = append(f.rolled, m.GetId())
	if f.failOn != "" && m.GetId() == f.failOn {
		return f.failErr
	}
	return nil
}

func (f *covFake) AppliedHead(_ context.Context) (string, error) { return f.head, f.headErr }
func (f *covFake) Close() error                                  { return f.closeErr }

// signedMig builds a plain Migration whose content_sha256 matches
// its (un-decorated) up body — the production wire shape now that
// migrations are fetched pre-verified (the client holds no verifier
// key). loadConnectionMigrations accepts it on the content-hash
// check alone.
func signedMig(t *testing.T, id, conn, body string) *applyfetchpb.Migration {
	t.Helper()
	down := "-- down for " + id
	return &applyfetchpb.Migration{
		Id:            id,
		Connection:    conn,
		UpSql:         body,
		DownSql:       down,
		ContentSha256: ContentHash(body, "", "", down),
	}
}

func seedMigs(t *testing.T, migs ...*applyfetchpb.Migration) string {
	t.Helper()
	dir := t.TempDir()
	for _, m := range migs {
		if err := WriteMigration(dir, m); err != nil {
			t.Fatalf("WriteMigration %s: %v", m.GetId(), err)
		}
	}
	return dir
}

// tgts / tgt build the per-connection apply ceilings the
// orchestrator reads (replacing the old lock proto).
func tgts(cts ...ConnTarget) []ConnTarget { return cts }

func tgt(name, target, hash string) ConnTarget {
	return ConnTarget{Connection: name, TargetMigrationID: target, TargetContentSha256: hash}
}

func applierForErr(err error) ApplierFor {
	return func(_ string) (Applier, error) { return nil, err }
}

// applierForNth returns an ApplierFor that hands back `a` for the
// first `okCalls` invocations, then errors. Used to let Plan open
// an applier successfully while the subsequent Run/RunRollback loop
// hits an ApplierFor failure.
func applierForNth(a Applier, okCalls int, err error) ApplierFor {
	n := 0
	return func(_ string) (Applier, error) {
		n++
		if n <= okCalls {
			return a, nil
		}
		return nil, err
	}
}

// ─── Plan error branches ────────────────────────────────────────

// TestPlan_LoadMigrationsError — a malformed artifact on disk for a
// pinned connection fails the load step; Plan wraps it with the
// connection name. INVARIANT: a corrupt artifact aborts planning.
func TestPlan_LoadMigrationsError(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Plan(context.Background(), Config{
		Targets: tgts(tgt("main", "ts-1", "")), MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return &covFake{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "connection main") {
		t.Errorf("expected connection-wrapped load error, got %v", err)
	}
}

// TestPlan_ApplierForError — ApplierFor failing for a pinned
// connection aborts Plan loud. INVARIANT: driver open failure is
// not silently skipped.
func TestPlan_ApplierForError(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	_, err := Plan(context.Background(), Config{
		Targets: tgts(tgt("main", m.GetId(), m.GetContentSha256())), MigrationsDir: dir,
		ApplierFor: applierForErr(errors.New("dial fail")),
	})
	if err == nil || !strings.Contains(err.Error(), "dial fail") {
		t.Errorf("expected applier open error, got %v", err)
	}
}

// TestPlan_CloseAfterHeadError — AppliedHead succeeds but Close
// errors; Plan surfaces the close error. INVARIANT: a leaked/
// failed driver close is reported, not swallowed.
func TestPlan_CloseAfterHeadError(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	fake := &covFake{closeErr: errors.New("close boom")}
	_, err := Plan(context.Background(), Config{
		Targets: tgts(tgt("main", m.GetId(), m.GetContentSha256())), MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "Close after AppliedHead") {
		t.Errorf("expected close-after-head error, got %v", err)
	}
}

// ─── Run error branches ─────────────────────────────────────────

// TestRun_PlanErrorPropagates — Run forwards a Plan validation
// error verbatim (here: empty MigrationsDir). INVARIANT: Run never
// starts the apply loop on an invalid plan.
func TestRun_PlanErrorPropagates(t *testing.T) {
	err := Run(context.Background(), Config{
		ApplierFor: func(_ string) (Applier, error) { return &covFake{}, nil }})
	if err == nil || !strings.Contains(err.Error(), "MigrationsDir is empty") {
		t.Errorf("expected MigrationsDir error from Run, got %v", err)
	}
}

// TestRun_ApplierForErrorInLoop — Plan opens the applier fine, but
// the apply loop's per-connection open fails; Run aborts.
// INVARIANT: a driver open failure during the apply loop stops the
// deploy.
func TestRun_ApplierForErrorInLoop(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	err := Run(context.Background(), Config{
		Targets: tgts(tgt("main", m.GetId(), m.GetContentSha256())), MigrationsDir: dir,
		ApplierFor: applierForNth(&covFake{}, 1, errors.New("loop dial fail")),
	})
	if err == nil || !strings.Contains(err.Error(), "loop dial fail") {
		t.Errorf("expected loop applier error, got %v", err)
	}
}

// ─── PlanRollback validation + error branches ───────────────────

// TestPlanRollback_Validation — empty MigrationsDir / nil ApplierFor
// each reject loud. INVARIANT: rollback refuses to run on an
// under-specified config.
func TestPlanRollback_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  RollbackConfig
		want string
	}{
		{"empty-dir", RollbackConfig{ApplierFor: applierForErr(nil)}, "MigrationsDir is empty"},
		{"nil-applierfor", RollbackConfig{MigrationsDir: "d"}, "ApplierFor is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanRollback(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// TestPlanRollback_LoadError — corrupt artifact aborts rollback
// planning with the connection name.
func TestPlanRollback_LoadError(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PlanRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return &covFake{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "connection main") {
		t.Errorf("expected load error, got %v", err)
	}
}

// TestPlanRollback_ApplierForError — driver open failure aborts.
func TestPlanRollback_ApplierForError(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "x")
	dir := seedMigs(t, m)
	_, err := PlanRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir,
		ApplierFor: applierForErr(errors.New("dial fail")),
	})
	if err == nil || !strings.Contains(err.Error(), "dial fail") {
		t.Errorf("expected applier open error, got %v", err)
	}
}

// TestPlanRollback_AppliedHeadError — DB-side head query failure
// aborts.
func TestPlanRollback_AppliedHeadError(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "x")
	dir := seedMigs(t, m)
	_, err := PlanRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) {
			return &covFake{headErr: errors.New("head boom")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "head boom") {
		t.Errorf("expected AppliedHead error, got %v", err)
	}
}

// TestPlanRollback_CloseError — Close after AppliedHead failing
// surfaces even on a fresh DB (head==""). INVARIANT: a failed close
// is reported.
func TestPlanRollback_CloseError(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "x")
	dir := seedMigs(t, m)
	_, err := PlanRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) {
			return &covFake{closeErr: errors.New("close boom")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Close after AppliedHead") {
		t.Errorf("expected close error, got %v", err)
	}
}

// TestPlanRollback_SkipsAboveHead — a disk migration newer than the
// applied head is NOT a rollback candidate (it was never applied).
// Also exercises the multi-id reverse-order filter. INVARIANT:
// rollback only touches migrations at or below AppliedHead.
func TestPlanRollback_SkipsAboveHead(t *testing.T) {
	m1 := signedMig(t, "ts-1", "main", "a")
	m2 := signedMig(t, "ts-2", "main", "b")
	m3 := signedMig(t, "ts-3", "main", "c")
	dir := seedMigs(t, m1, m2, m3)
	pending, err := PlanRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (Applier, error) { return &covFake{head: "ts-2"}, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	// ts-3 above head → skipped; ts-2, ts-1 rolled back in reverse.
	if len(pending) != 2 || pending[0].Migration.GetId() != "ts-2" || pending[1].Migration.GetId() != "ts-1" {
		t.Errorf("expected [ts-2, ts-1], got %+v", pending)
	}
}

// TestPlanRollback_MultiConnectionOrder — two connections sort lex
// before planning. INVARIANT: deterministic connection order.
func TestPlanRollback_MultiConnectionOrder(t *testing.T) {
	a := signedMig(t, "a-1", "alpha", "a")
	b := signedMig(t, "b-1", "beta", "b")
	dir := seedMigs(t, a, b)
	pending, err := PlanRollback(context.Background(), RollbackConfig{
		Targets:       tgts(tgt("beta", "", ""), tgt("alpha", "", "")),
		MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(name string) (Applier, error) { return &covFake{head: name[:1] + "-1"}, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(pending) != 2 || pending[0].Connection != "alpha" || pending[1].Connection != "beta" {
		t.Errorf("expected alpha before beta, got %+v", pending)
	}
}

// ─── RunRollback error branches ─────────────────────────────────

// TestRunRollback_NilOutDiscards — nil Out defaults to io.Discard;
// no panic. INVARIANT: Out is optional.
func TestRunRollback_NilOutDiscards(t *testing.T) {
	err := RunRollback(context.Background(), RollbackConfig{
		MigrationsDir: t.TempDir(),
		ApplierFor:    func(_ string) (Applier, error) { return &covFake{}, nil },
	})
	if err != nil {
		t.Errorf("RunRollback nil Out: %v", err)
	}
}

// TestRunRollback_PlanErrorPropagates — RunRollback forwards a
// PlanRollback validation error (empty MigrationsDir).
func TestRunRollback_PlanErrorPropagates(t *testing.T) {
	err := RunRollback(context.Background(), RollbackConfig{
		ApplierFor: func(_ string) (Applier, error) { return &covFake{}, nil }})
	if err == nil || !strings.Contains(err.Error(), "MigrationsDir is empty") {
		t.Errorf("expected MigrationsDir error, got %v", err)
	}
}

// TestRunRollback_ApplierForErrorInLoop — Plan opens fine; the
// rollback loop's open fails. INVARIANT: open failure stops the
// destructive loop.
func TestRunRollback_ApplierForErrorInLoop(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "x")
	dir := seedMigs(t, m)
	err := RunRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: applierForNth(&covFake{head: "ts-1"}, 1, errors.New("loop dial fail")),
	})
	if err == nil || !strings.Contains(err.Error(), "loop dial fail") {
		t.Errorf("expected loop applier error, got %v", err)
	}
}

// TestRunRollback_RollbackErrorAborts — the driver's Rollback
// fails mid-list; RunRollback aborts loud. Also exercises the
// per-connection applier cache reuse (two pending, same conn).
// INVARIANT: a rollback failure stops the loop.
func TestRunRollback_RollbackErrorAborts(t *testing.T) {
	m1 := signedMig(t, "ts-1", "main", "a")
	m2 := signedMig(t, "ts-2", "main", "b")
	dir := seedMigs(t, m1, m2)
	fake := &covFake{head: "ts-2", failOn: "ts-2", failErr: errors.New("rollback boom")}
	err := RunRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "rollback boom") {
		t.Errorf("expected rollback failure, got %v", err)
	}
}

// TestRunRollback_CacheReuse — two pending on the same connection
// share one opened applier. INVARIANT: a connection's driver is
// opened once per rollback run.
func TestRunRollback_CacheReuse(t *testing.T) {
	m1 := signedMig(t, "ts-1", "main", "a")
	m2 := signedMig(t, "ts-2", "main", "b")
	dir := seedMigs(t, m1, m2)
	fake := &covFake{head: "ts-2"}
	opens := 0
	err := RunRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (Applier, error) { opens++; return fake, nil },
	})
	if err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	if len(fake.rolled) != 2 || fake.rolled[0] != "ts-2" || fake.rolled[1] != "ts-1" {
		t.Errorf("expected reverse rollback [ts-2, ts-1], got %v", fake.rolled)
	}
	// PlanRollback opens once (head), the loop opens once (cached
	// across both migrations) — two total, not three.
	if opens != 2 {
		t.Errorf("expected 2 opens (plan + one cached loop open), got %d", opens)
	}
}

// ─── newLogger ──────────────────────────────────────────────────

// TestNewLogger_NilWriterAndFormats — nil writer falls back to
// io.Discard; both json + text handlers build without panic.
// INVARIANT: newLogger never returns nil / never panics on a nil
// sink.
func TestNewLogger_NilWriterAndFormats(t *testing.T) {
	for _, format := range []string{"", "text", "json"} {
		if l := newLogger(nil, format); l == nil {
			t.Errorf("format %q: newLogger returned nil", format)
		}
	}
}

// ─── captureMigrationError (Sentry-initialised path) ────────────

// TestCaptureMigrationError_WithScope — with a Sentry client bound,
// captureMigrationError tags + captures the event. INVARIANT: a
// failed migration is reported to Sentry when initialised; no-op
// otherwise.
func TestCaptureMigrationError_WithScope(t *testing.T) {
	// Empty-DSN client is non-nil but transport-disabled — exercises
	// the WithScope/SetTag path without any network I/O.
	client, err := sentry.NewClient(sentry.ClientOptions{})
	if err != nil {
		t.Fatalf("sentry.NewClient: %v", err)
	}
	hub := sentry.CurrentHub()
	old := hub.Client()
	hub.BindClient(client)
	t.Cleanup(func() { hub.BindClient(old) })

	m := &applyfetchpb.Migration{Id: "ts-1"}
	captureMigrationError("apply", "main", m, errors.New("boom"))
	// No assertion beyond "did not panic": the WithScope closure +
	// SetTag calls are the lines under coverage.
}

// ─── loadConnectionMigrations (unit) ────────────────────────────

// TestLoadConnectionMigrations_MissingDirIsEmpty — a connection
// that never fetched returns (nil, nil), not an error.
func TestLoadConnectionMigrations_MissingDirIsEmpty(t *testing.T) {
	migs, err := loadConnectionMigrations(t.TempDir(), "never-fetched")
	if err != nil || migs != nil {
		t.Errorf("missing dir should yield (nil, nil); got %v, %v", migs, err)
	}
}

// TestLoadConnectionMigrations_ReadDirError — a non-ErrNotExist
// stat error (the connection path is a regular file, not a dir)
// surfaces. INVARIANT: a real I/O error is not masked as "fresh".
func TestLoadConnectionMigrations_ReadDirError(t *testing.T) {
	root := t.TempDir()
	// Make <root>/main a FILE so ReadDir errors with ENOTDIR.
	if err := os.WriteFile(filepath.Join(root, "main"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadConnectionMigrations(root, "main")
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("expected read error, got %v", err)
	}
}

// TestLoadConnectionMigrations_SkipsNonJSONAndDirs — a sub-dir and
// a non-.json file in the connection dir are ignored. INVARIANT:
// only top-level *.json artifacts are loaded.
func TestLoadConnectionMigrations_SkipsNonJSONAndDirs(t *testing.T) {
	root := t.TempDir()
	cdir := filepath.Join(root, "main")
	if err := os.MkdirAll(filepath.Join(cdir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.up.sql"), []byte("not loaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := signedMig(t, "ts-1", "main", "x")
	if err := WriteMigration(root, m); err != nil {
		t.Fatal(err)
	}
	migs, err := loadConnectionMigrations(root, "main")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(migs) != 1 || migs[0].GetId() != "ts-1" {
		t.Errorf("expected only ts-1.json loaded, got %+v", migs)
	}
}

// TestLoadConnectionMigrations_ReadFileError — an unreadable .json
// (perms 0000) surfaces a read error. INVARIANT: an I/O failure on
// a candidate artifact aborts the load.
func TestLoadConnectionMigrations_ReadFileError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: 0000 perms don't deny reads")
	}
	root := t.TempDir()
	cdir := filepath.Join(root, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cdir, "ts-1.json")
	if err := os.WriteFile(p, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	_, err := loadConnectionMigrations(root, "main")
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("expected read error, got %v", err)
	}
}

// TestLoadConnectionMigrations_ParseError — malformed protojson is
// rejected with a parse error.
func TestLoadConnectionMigrations_ParseError(t *testing.T) {
	root := t.TempDir()
	cdir := filepath.Join(root, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.json"), []byte("{not valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadConnectionMigrations(root, "main")
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got %v", err)
	}
}

// TestLoadConnectionMigrations_HashMismatch — a valid protojson
// whose up_sql doesn't match content_sha256 is rejected (someone
// hand-edited the body). INVARIANT: artifact integrity is checked
// at load.
func TestLoadConnectionMigrations_HashMismatch(t *testing.T) {
	root := t.TempDir()
	cdir := filepath.Join(root, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &applyfetchpb.Migration{
		Id: "ts-1", Connection: "main",
		UpSql:         "real body",
		ContentSha256: strings.Repeat("0", 64), // wrong
	}
	buf, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.json"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = loadConnectionMigrations(root, "main")
	if err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Errorf("expected hash mismatch, got %v", err)
	}
}

// TestLoadConnectionMigrations_BackfillsConnectionName — an older
// artifact that didn't stamp connection gets it self-healed from
// the directory name. INVARIANT: missing connection is backfilled,
// not fatal.
func TestLoadConnectionMigrations_BackfillsConnectionName(t *testing.T) {
	root := t.TempDir()
	cdir := filepath.Join(root, "main")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "CREATE TABLE x;"
	m := &applyfetchpb.Migration{
		Id:            "ts-1",
		UpSql:         body,
		ContentSha256: ContentHash(body, "", "", ""),
		// Connection intentionally empty.
	}
	buf, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "ts-1.json"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	migs, err := loadConnectionMigrations(root, "main")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(migs) != 1 || migs[0].GetConnection() != "main" {
		t.Errorf("expected connection backfilled to 'main', got %+v", migs)
	}
}

// ─── WriteMigration error branches ──────────────────────────────

// TestWriteMigration_MkdirAllError — when the root is a regular
// file, the per-connection MkdirAll fails. INVARIANT: a layout
// directory that can't be created aborts the write.
func TestWriteMigration_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	rootFile := filepath.Join(tmp, "root-is-a-file")
	if err := os.WriteFile(rootFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := signedMig(t, "ts-1", "main", "x")
	err := WriteMigration(rootFile, m)
	if err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("expected mkdir error, got %v", err)
	}
}

// TestWriteMigration_FileWriteErrors — when a target output path is
// pre-occupied by a directory, the corresponding os.WriteFile
// fails. Three sub-cases cover the .json, .up.sql, and .down.sql
// writes. INVARIANT: a write failure on any of the three artifacts
// aborts.
func TestWriteMigration_FileWriteErrors(t *testing.T) {
	cases := []struct {
		name     string
		occupied string // filename pre-created as a directory
	}{
		{"json", "ts-1.json"},
		{"up-sql", "ts-1.up.sql"},
		{"down-sql", "ts-1.down.sql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// Pre-create the target path as a directory so the matching
			// WriteFile call fails with EISDIR. MkdirAll also creates
			// <root>/main, so the earlier WriteFile calls succeed.
			if err := os.MkdirAll(filepath.Join(root, "main", tc.occupied), 0o755); err != nil {
				t.Fatal(err)
			}
			m := signedMig(t, "ts-1", "main", "x")
			if err := WriteMigration(root, m); err == nil {
				t.Errorf("expected write error when %s is a directory", tc.occupied)
			}
		})
	}
}

// Compile-time assertion the white-box fake satisfies Applier.
var _ Applier = (*covFake)(nil)

// silence unused import if io ever drops out of the file.
var _ = io.Discard
