package migrate_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// Two bundles of one project fetch their own connections into one directory:
// the second fetch must not erase the first one's replaced entries, and a
// connection fetched with nothing replaced is recorded as asked.
func TestWriteReplaced_KeepsOtherConnectionsAndRecordsEmptyOnes(t *testing.T) {
	dir := t.TempDir()
	if err := migrate.WriteReplaced(dir, []string{"app"}, map[string]map[string]string{"app": {"ts-1": "ts-2"}}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.WriteReplaced(dir, []string{"billing"}, nil); err != nil {
		t.Fatal(err)
	}
	got, present, err := migrate.ReadReplaced(os.DirFS(dir))
	if err != nil || !present {
		t.Fatalf("ReadReplaced: present=%v err=%v", present, err)
	}
	want := map[string]map[string]string{"app": {"ts-1": "ts-2"}, "billing": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	if miss, err := migrate.UnrecordedConnections(os.DirFS(dir), []string{"billing", "app", "cache"}); err != nil || !reflect.DeepEqual(miss, []string{"cache"}) {
		t.Errorf("unrecorded = %v (%v), want [cache]", miss, err)
	}
}

// A list that is there but cannot be parsed refuses the plan: read as
// "nothing replaced" it would switch the guard off in silence.
func TestPlan_RefusesAnUnreadableReplacedList(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x (id BIGINT);")
	dir := seedDir(t, m)
	if err := os.WriteFile(filepath.Join(dir, migrate.ReplacedFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Plan(t.Context(), migrate.Config{
		Targets:       targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), migrate.ReplacedFileName) {
		t.Fatalf("want a refusal naming %s, got %v", migrate.ReplacedFileName, err)
	}
}

// The list on disk is what Plan judges a directory by when the caller passes
// none: a replaced first head is refused on the `--migrations` path too.
func TestPlan_ReadsTheReplacedListBesideTheArtefacts(t *testing.T) {
	repl := mkMig("ts-3", "main", "CREATE TABLE x (id BIGINT, name TEXT);")
	dir := seedDir(t, repl)
	if err := migrate.WriteReplaced(dir, []string{"main"}, map[string]map[string]string{"main": {"ts-1": "ts-3"}}); err != nil {
		t.Fatal(err)
	}
	stubA := stub.New()
	stubA.Head = "ts-1"
	_, err := migrate.Plan(t.Context(), migrate.Config{
		Targets:       targets(lockTarget{"main", repl.GetId(), repl.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "replaced by ts-3") {
		t.Fatalf("want the replaced-head refusal from the list on disk, got %v", err)
	}
}
