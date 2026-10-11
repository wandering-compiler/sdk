package migrate_test

import (
	"os"
	"path/filepath"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// The connection name and migration id of a fetched artifact come from the
// console and are not covered by the content hash. They become path segments,
// so anything that is not a single plain name must be refused before a byte
// is written — and nothing may land outside the migrations root.
func TestWriteMigration_RefusesPathShapedNames(t *testing.T) {
	cases := []struct {
		name     string
		id, conn string
	}{
		{"id climbs out", "../../escaped", "main"},
		{"id nests", "sub/20260101T000000Z", "main"},
		{"id backslash", `..\escaped`, "main"},
		{"id dot-dot", "..", "main"},
		{"id control char", "20260101T000000Z\n", "main"},
		{"connection climbs out", "20260101T000000Z", "../../escaped"},
		{"connection absolute", "20260101T000000Z", "/etc"},
		{"connection dot", "20260101T000000Z", "."},
		{"connection dot-dot", "20260101T000000Z", ".."},
		{"connection nests", "20260101T000000Z", "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "a", "b")
			m := mkMig(tc.id, tc.conn, "SELECT 1;")
			if err := migrate.WriteMigration(root, m); err == nil {
				t.Fatalf("WriteMigration accepted id=%q connection=%q", tc.id, tc.conn)
			}
			if _, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{m}); err == nil {
				t.Errorf("MigrationSetFS accepted id=%q connection=%q", tc.id, tc.conn)
			}
			// Nothing at all may have been written, inside the root or out.
			_ = filepath.Walk(parent, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					t.Errorf("file written despite refusal: %s", p)
				}
				return nil
			})
		})
	}
}

// The ordinary shape keeps working and lands where the loader looks.
func TestWriteMigration_PlainNamesLandUnderRoot(t *testing.T) {
	root := t.TempDir()
	m := mkMig("20260101T000000Z", "orders-db", "SELECT 1;")
	if err := migrate.WriteMigration(root, m); err != nil {
		t.Fatalf("WriteMigration: %v", err)
	}
	for _, ext := range []string{".json", ".up.sql", ".down.sql"} {
		p := filepath.Join(root, "orders-db", "20260101T000000Z"+ext)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}
