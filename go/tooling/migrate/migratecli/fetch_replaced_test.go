package migratecli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	_ "modernc.org/sqlite"
)

// replMig renders a body the way the console does: the DDL plus the ledger
// row (the ledger is written by up_sql, not by the applier).
func replMig(id, up string) *applyfetchpb.Migration {
	up = "CREATE TABLE IF NOT EXISTS w17_migrations (timestamp TEXT PRIMARY KEY); " + up +
		" INSERT INTO w17_migrations (timestamp) VALUES ('" + id + "');"
	m := &applyfetchpb.Migration{Id: id, Connection: "main", UpSql: up, DownSql: "-- down " + id}
	m.ContentSha256 = migrate.ContentHashWithChecks(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", "", "", nil)
	return m
}

func sqliteTables(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedReplacedFirstHead applies M1 — the first and only migration on `main` —
// to a real SQLite database, then serves a console that REPLACED it with M2
// (M2 roots the live history: no predecessor, no supersedes). It returns the
// console's fake, the lock pinning M2, the options and the database path.
func seedReplacedFirstHead(t *testing.T, ctx context.Context) (*fakeFetch, string, Options, *bytes.Buffer, string, map[string]string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	env := map[string]string{"W17_TARGET_MAIN": "sqlite://" + dbPath}
	out := &bytes.Buffer{}
	opts := Options{Out: out, Getenv: fetchEnv(env)}

	m1 := replMig("20260101T000000Z", "CREATE TABLE x (id INTEGER);")
	dirA := filepath.Join(t.TempDir(), "migA")
	if err := migrate.WriteMigration(dirA, m1); err != nil {
		t.Fatal(err)
	}
	lockA := writeLock(t, migrate.LockConnection{Name: "main", TargetMigrationID: m1.GetId(), TargetContentSha256: m1.GetContentSha256()})
	if err := Main(ctx, []string{"apply", "--lock", lockA, "--migrations", dirA}, opts); err != nil {
		t.Fatalf("seed apply: %v\n%s", err, out.String())
	}

	m2 := replMig("20260102T000000Z", "CREATE TABLE y (id INTEGER);")
	f := &fakeFetch{resp: &applyfetchpb.FetchMigrationsResponse{
		Migrations: []*applyfetchpb.Migration{m2},
		Replaced:   []*applyfetchpb.ReplacedMigration{{Connection: "main", MigrationId: m1.GetId(), ReplacedBy: m2.GetId()}},
	}}
	env[envConsoleAddr] = "grpc://" + serveFetch(t, f, nil)
	lockB := writeLock(t, migrate.LockConnection{Name: "main", TargetMigrationID: m2.GetId(), TargetContentSha256: m2.GetContentSha256()})
	return f, lockB, opts, out, dbPath, env
}

// The documented two-step deployment — `migrate fetch` then `migrate apply
// --migrations` — must refuse a database whose applied head the console
// replaced exactly as `apply --fetch` does, including when the replaced
// migration was the FIRST on its connection (the case only the console's
// replaced list can tell from an unknown past). The list travels with the
// fetched artefacts, in a file beside the connection directories.
func TestFetchThenApply_RefusesAHeadTheConsoleReplaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, lockB, opts, out, dbPath, _ := seedReplacedFirstHead(t, ctx)

	dir := filepath.Join(t.TempDir(), "migB")
	out.Reset()
	if err := Main(ctx, []string{"fetch", "--lock", lockB, "--migrations", dir}, opts); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, migrate.ReplacedFileName)); err != nil {
		t.Fatalf("migrate fetch wrote no replaced list beside the artefacts: %v", err)
	}

	out.Reset()
	err := Main(ctx, []string{"apply", "--lock", lockB, "--migrations", dir}, opts)
	if err == nil || !strings.Contains(err.Error(), "replaced by 20260102T000000Z") {
		t.Fatalf("apply --migrations ran the replacement on top of the replaced first head: err=%v\n%s", err, out.String())
	}
	if tables := sqliteTables(t, dbPath); slices.Contains(tables, "y") {
		t.Errorf("the replacement's DDL reached the database: tables=%v", tables)
	}

	// sdk-tooling-12: a fetch that can read the database reports its head, so
	// the console's replace guard hears from this deployment too.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heads) == 0 || len(f.heads[0]) != 1 || f.heads[0][0].GetMigrationId() != "20260101T000000Z" || f.heads[0][0].GetDatabaseId() == "" {
		t.Errorf("migrate fetch reported heads %v, want main@20260101T000000Z with a database id", f.heads)
	}
}

// A fetch on a workstation holds no DSN: it reports nothing for that
// connection and still writes the artefacts and the list.
func TestFetch_WithoutADSNReportsNoHeadAndStillWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, lockB, _, out, _, env := seedReplacedFirstHead(t, ctx)
	ws := Options{Out: out, Getenv: fetchEnv(map[string]string{envConsoleAddr: env[envConsoleAddr]})}
	dir := filepath.Join(t.TempDir(), "migB")
	if err := Main(ctx, []string{"fetch", "--lock", lockB, "--migrations", dir}, ws); err != nil {
		t.Fatalf("fetch without a DSN: %v\n%s", err, out.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heads) != 1 || len(f.heads[0]) != 0 {
		t.Errorf("heads reported without a DSN: %v", f.heads)
	}
	if _, err := os.Stat(filepath.Join(dir, migrate.ReplacedFileName)); err != nil {
		t.Errorf("no replaced list written: %v", err)
	}
}

// A directory fetched by an older binary carries no list. It still applies —
// the off-chain refusal from the ids alone still holds — but the operator is
// told, by name, which guard is missing and how to get it back.
func TestApplyMigrations_DirWithoutAReplacedListWarns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	var out bytes.Buffer
	opts := Options{Out: &out, Getenv: fetchEnv(map[string]string{"W17_TARGET_MAIN": "sqlite://" + dbPath})}
	m1 := replMig("20260101T000000Z", "CREATE TABLE x (id INTEGER);")
	dir := filepath.Join(t.TempDir(), "old")
	if err := migrate.WriteMigration(dir, m1); err != nil {
		t.Fatal(err)
	}
	lock := writeLock(t, migrate.LockConnection{Name: "main", TargetMigrationID: m1.GetId(), TargetContentSha256: m1.GetContentSha256()})
	if err := Main(ctx, []string{"apply", "--lock", lock, "--migrations", dir}, opts); err != nil {
		t.Fatalf("an old fetched directory no longer applies: %v\n%s", err, out.String())
	}
	for _, want := range []string{"WARNING", migrate.ReplacedFileName, "main", "migrate fetch"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the warning does not say %q:\n%s", want, out.String())
		}
	}
}
