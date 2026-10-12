package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
)

// Every connection the applier's pool opens carries the shared session
// settings and the collation the compiler names on string columns — not
// only the first: foreign keys are enforced, LIKE is case-sensitive (as in
// PostgreSQL), and W17_UNICODE compares 'a' and 'A' as different.
func TestNew_EveryPooledConnectionCarriesThePinsAndTheCollation(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, "file:"+filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	first, err := a.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := a.db.Conn(ctx) // held together: a distinct connection
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	for i, conn := range []*sql.Conn{first, second} {
		var fk, like, eq int
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(ctx, `SELECT 'a' LIKE 'A'`).Scan(&like); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(ctx, `SELECT 'a' = 'A' COLLATE `+sqlitecollate.Name).Scan(&eq); err != nil {
			t.Fatalf("connection %d: the collation is not registered: %v", i, err)
		}
		if fk != 1 || like != 0 || eq != 0 {
			t.Errorf("connection %d: foreign_keys=%d, 'a' LIKE 'A'=%d, 'a'='A' under %s=%d; want 1, 0, 0", i, fk, like, sqlitecollate.Name, eq)
		}
	}
}
