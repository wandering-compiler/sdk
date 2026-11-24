package migrate

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A migration set on a volume another container applies from is rewritten by
// `migrate fetch` while that container may be reading it. The artefact is
// replaced whole (written aside, then renamed), so a reader sees the old file
// or the new one — never a truncated, half-written one. Proven with a reader
// that opened the file before the rewrite: an in-place rewrite truncates the
// file under it.
func TestWriteMigration_ReplacesTheArtefactWhole(t *testing.T) {
	dir := t.TempDir()
	m := signedMig(t, "ts-1", "main", "CREATE TABLE a;")
	if err := WriteMigration(dir, m); err != nil {
		t.Fatalf("WriteMigration: %v", err)
	}
	path := filepath.Join(dir, "main", "ts-1.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	m2 := signedMig(t, "ts-1", "main", "CREATE TABLE a (id bigint primary key, note text);")
	if err := WriteMigration(dir, m2); err != nil {
		t.Fatalf("WriteMigration (rewrite): %v", err)
	}
	seen, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != string(before) {
		t.Fatalf("a reader that opened the artefact before the rewrite read %d bytes of something else (the file was rewritten in place under it):\n%s", len(seen), seen)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "note text") {
		t.Fatalf("the rewrite did not land:\n%s", after)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the connection directory holds %v, want the three artefact files and nothing left over", names)
	}
}
