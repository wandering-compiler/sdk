package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// F8-D-3: `VACUUM INTO` replays the schema DDL, so a DB whose string columns
// carry `COLLATE W17_UNICODE` (every generated SQLite project since F7-A-5) can
// only be dumped if the Snapshotter's own connection registers the collation.
// Before the fix, Dump failed "no such collation sequence: W17_UNICODE".
func TestSnapshotter_DumpsUnicodeCollatedDB(t *testing.T) {
	ctx := context.Background()
	// The collation must exist to CREATE the seed table; Dump then registers it
	// again on its own connection (the fix under test).
	sqlitecollate.Register()

	path := filepath.Join(t.TempDir(), "collated.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (name TEXT COLLATE `+sqlitecollate.Name+` NOT NULL)`); err != nil {
		t.Fatalf("create collated table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t(name) VALUES ('café'),('Zoe')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	snap, err := sqlite.NewSnapshotter("sqlite://" + path)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump of a W17_UNICODE-collated DB must not fail on the collation: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump produced no output")
	}
}

func TestNewSnapshotter_EmptyDSNRefuses(t *testing.T) {
	_, err := sqlite.NewSnapshotter("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestNewSnapshotter_InMemoryRefuses(t *testing.T) {
	for _, dsn := range []string{
		"file::memory:",
		"sqlite://:memory:",
		"file:test.db?mode=memory&cache=shared",
	} {
		if _, err := sqlite.NewSnapshotter(dsn); err == nil || !strings.Contains(err.Error(), "no file to snapshot") {
			t.Errorf("dsn %q: expected in-memory refusal, got %v", dsn, err)
		}
	}
}

// TestSnapshotter_RoundTrip is the S2 sqlite verify: dump → wipe →
// restore → fingerprint equal + data spot-check. Runs locally — the
// modernc.org/sqlite driver is pure Go and the store is a file, so no
// docker is needed.
func TestSnapshotter_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	dsn := "sqlite://" + path

	open := func() *sql.DB {
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return db
	}

	db := open()
	seed := []string{
		`CREATE TABLE widget (id INTEGER PRIMARY KEY, name TEXT NOT NULL, qty INTEGER DEFAULT 0)`,
		`CREATE INDEX widget_name_idx ON widget (name)`,
		`INSERT INTO widget (id, name, qty) VALUES (1, 'a', 3), (2, 'b', 7)`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	before, err := fingerprint.ExtractSQLite(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}
	_ = db.Close()

	snap, err := sqlite.NewSnapshotter(dsn)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump produced no output")
	}

	// Wipe: drop the table so the schema diverges.
	db = open()
	if _, err := db.ExecContext(ctx, `DROP TABLE widget`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	wiped, err := fingerprint.ExtractSQLite(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint wiped: %v", err)
	}
	if wiped.FingerprintHex() == before.FingerprintHex() {
		t.Fatal("wipe did not change the fingerprint — setup is wrong")
	}
	_ = db.Close()

	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	db = open()
	defer func() { _ = db.Close() }()
	after, err := fingerprint.ExtractSQLite(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if after.FingerprintHex() != before.FingerprintHex() {
		t.Fatalf("fingerprint mismatch after round-trip:\n before=%s\n  after=%s", before.FingerprintHex(), after.FingerprintHex())
	}
	var qty int
	if err := db.QueryRowContext(ctx, `SELECT qty FROM widget WHERE id = 2`).Scan(&qty); err != nil {
		t.Fatalf("post-restore data read: %v", err)
	}
	if qty != 7 {
		t.Errorf("restored qty = %d, want 7", qty)
	}
}
