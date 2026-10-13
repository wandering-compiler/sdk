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

// fakeMig is a migration --fake can record: it carries adopt_sql and a
// preflight. bind selects the digest formula — true binds the preflight
// (ContentHashWithPreflight), false is the formula that predates the binding
// (ContentHash), which every artifact minted before it carries.
func fakeMig(t *testing.T, bind bool) (dir string, m *applyfetchpb.Migration) {
	t.Helper()
	m = &applyfetchpb.Migration{
		Id:                "20260101T000000Z",
		Connection:        "main",
		UpSql:             "CREATE TABLE t(id int);",
		DownSql:           "DROP TABLE t;",
		AdoptSql:          "INSERT INTO ledger VALUES ('20260101T000000Z');",
		AdoptPreflightSql: "SELECT must_hold();",
	}
	if bind {
		m.ContentSha256 = migrate.ContentHashWithPreflight(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, m.GetAdoptSql(), m.GetAdoptPreflightSql(), "")
	} else {
		m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, m.GetAdoptSql(), "")
	}
	return seedDirUnchained(t, m), m
}

// replacePreflight edits the on-disk artifact's preflight and nothing else —
// the digest stays as it was.
func replacePreflight(t *testing.T, dir string, m *applyfetchpb.Migration, preflight string) {
	t.Helper()
	p := filepath.Join(dir, m.GetConnection(), m.GetId()+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk applyfetchpb.Migration
	if err := protojson.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk.AdoptPreflightSql = preflight
	out, err := protojson.Marshal(&onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runFake(dir string, m *applyfetchpb.Migration) (*stub.Applier, error) {
	st := stub.New()
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       []migrate.ConnTarget{{Connection: m.GetConnection(), TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
		Fake:          true,
		Out:           &strings.Builder{},
	})
	return st, err
}

// --fake EXECUTES the preflight, so a preflight the digest does not cover must
// not run: replacing it would run arbitrary statements, or license a record
// with a check that always passes.
func TestFake_RefusesAPreflightTheDigestDoesNotCover(t *testing.T) {
	dir, m := fakeMig(t, false)
	replacePreflight(t, dir, m, "DROP TABLE customers;")
	st, err := runFake(dir, m)
	if err == nil {
		t.Fatal("--fake ran an adopt preflight its content_sha256 does not cover")
	}
	if !strings.Contains(err.Error(), "does not cover its adopt preflight") {
		t.Errorf("refusal does not name the cause: %v", err)
	}
	// Re-fetching returns the stored digest unchanged, so advice that stops
	// at "fetch again" sends the operator round in a circle.
	if !strings.Contains(err.Error(), "squash") || !strings.Contains(err.Error(), "Apply it for real") {
		t.Errorf("refusal does not name a way out that works for a stored migration: %v", err)
	}
	if n := len(st.Calls()); n != 0 {
		t.Errorf("%d body(ies) executed despite the refusal", n)
	}
}

// With a binding digest, replacing the preflight is a content mismatch like any
// other edit to a body that runs.
func TestFake_BoundPreflightCannotBeReplaced(t *testing.T) {
	dir, m := fakeMig(t, true)
	replacePreflight(t, dir, m, "SELECT 1;")
	st, err := runFake(dir, m)
	if err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Fatalf("replaced bound preflight was not refused as a mismatch: %v", err)
	}
	if n := len(st.Calls()); n != 0 {
		t.Errorf("%d body(ies) executed despite the refusal", n)
	}
}

// Stripping a bound preflight is refused too — the digest no longer matches,
// rather than quietly falling back to the formula without the preflight.
func TestFake_BoundPreflightCannotBeStripped(t *testing.T) {
	dir, m := fakeMig(t, true)
	replacePreflight(t, dir, m, "")
	if _, err := runFake(dir, m); err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Fatalf("stripped bound preflight was not refused as a mismatch: %v", err)
	}
}

// The untampered, bound artifact still records: preflight and ledger write in
// one body, in that order.
func TestFake_BoundPreflightRuns(t *testing.T) {
	dir, m := fakeMig(t, true)
	st, err := runFake(dir, m)
	if err != nil {
		t.Fatalf("--fake on a bound artifact: %v", err)
	}
	calls := st.Calls()
	if len(calls) != 1 || calls[0].GetUpSql() != m.GetAdoptPreflightSql()+"\n"+m.GetAdoptSql() {
		t.Fatalf("unexpected bodies: %v", calls)
	}
}

// An artifact minted before the binding still loads and applies normally — the
// preflight only runs under --fake, so nothing else may change for it.
func TestApply_UnboundPreflightStillAppliesForReal(t *testing.T) {
	dir, m := fakeMig(t, false)
	st := stub.New()
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
		Out:           &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("apply of a pre-binding artifact: %v", err)
	}
	if calls := st.Calls(); len(calls) != 1 || calls[0].GetUpSql() != m.GetUpSql() {
		t.Fatalf("unexpected bodies: %v", calls)
	}
}

// A migration without a preflight hashes exactly as before, under either
// function — no existing digest or lock pin moves.
func TestContentHashWithPreflight_EmptyPreflightKeepsTheDigest(t *testing.T) {
	a := migrate.ContentHash("SELECT 1;", "", "", "x", "p", []string{"s"}, "adopt", `{"required_extensions":["vector"]}`)
	b := migrate.ContentHashWithPreflight("SELECT 1;", "", "", "x", "p", []string{"s"}, "adopt", "", `{"required_extensions":["vector"]}`)
	if a != b {
		t.Fatal("an empty preflight changed the digest")
	}
	if c := migrate.ContentHashWithPreflight("SELECT 1;", "", "", "x", "p", []string{"s"}, "adopt", "SELECT 2;", `{"required_extensions":["vector"]}`); c == a {
		t.Fatal("a preflight does not move the digest")
	}
}
