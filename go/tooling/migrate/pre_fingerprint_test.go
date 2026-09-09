package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// hdr builds a forward body carrying the expected_pre_fingerprint header the
// console stamps, in the position it stamps it: the first line.
func hdr(pre, body string) string {
	return "-- wc:expected_pre_fingerprint: " + pre + "\n" + body
}

// runOne drives a single-migration apply through the orchestrator against a
// stub whose Fingerprint answers `targetFp`, and returns the error.
func runOne(t *testing.T, m *applyfetchpb.Migration, targetFp string, fpErr error) error {
	t.Helper()
	dir := seedDir(t, m)
	stubA := stub.New()
	stubA.Fp = targetFp
	stubA.FpErr = fpErr
	return migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
}

// The gate REFUSES when the database is not in the state the migration was
// planned against. This is the whole point of the header, and until now
// nothing compared it — the value was stamped, parsed, carried across a
// rescope, and never confronted with a database.
func TestPreFingerprint_RefusesWhenTargetDisagrees(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	err := runOne(t, m, "bbbb2222", nil)
	if err == nil {
		t.Fatal("apply must refuse when the target's schema state disagrees with the migration's expected pre-state")
	}
	// Assert on the diagnostic, not merely on non-nil: a refusal that names
	// neither value is one an operator cannot act on, and this test would
	// otherwise pass for any unrelated failure.
	for _, want := range []string{"aaaa1111", "bbbb2222", "not in the state this migration was planned against"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// The migration must NOT have run. A drift check that refuses after applying
// the body is a report, not a gate.
func TestPreFingerprint_RefusalHappensBeforeTheBodyRuns(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	dir := seedDir(t, m)
	stubA := stub.New()
	stubA.Fp = "bbbb2222"
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("want refusal")
	}
	if n := len(stubA.Calls()); n != 0 {
		t.Errorf("body was applied despite the drift refusal (%d Apply call(s))", n)
	}
}

// Agreement applies normally — the gate must not be a blanket refusal that
// happens to look right in the failing case.
func TestPreFingerprint_AppliesWhenTargetAgrees(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	if err := runOne(t, m, "aaaa1111", nil); err != nil {
		t.Fatalf("matching fingerprints must apply: %v", err)
	}
}

// The `FAKE_` placeholder is not a fingerprint of anything. The console stamps
// it for dialects its shadow-DB matrix does not cover, so comparing against it
// would refuse every migration on those connections — the gate turning into an
// outage for the case it has no opinion about.
func TestPreFingerprint_SkipsFakePlaceholder(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("FAKE_0123456789abcdef", "CREATE TABLE x;"))
	if err := runOne(t, m, "bbbb2222", nil); err != nil {
		t.Fatalf("a FAKE_ placeholder must not be compared: %v", err)
	}
}

// A body with no header at all still applies: header presence is enforced by
// the console when it SERVES the migration, and refusing here would break
// dev-mode applies without making any fetched migration safer.
func TestPreFingerprint_SkipsWhenNoHeader(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	if err := runOne(t, m, "bbbb2222", nil); err != nil {
		t.Fatalf("a body with no header must not be refused: %v", err)
	}
}

// An empty fingerprint is the contract's "no applicable schema" — a fresh
// database. That is not disagreement.
func TestPreFingerprint_SkipsEmptyTargetFingerprint(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	if err := runOne(t, m, "", nil); err != nil {
		t.Fatalf("an empty target fingerprint means a fresh DB, not drift: %v", err)
	}
}

// Failing to READ the target's state must not be mistaken for agreement. This
// is the arm where a silent `return nil` would be most tempting and most
// wrong: the gate would go quiet exactly when it cannot see.
func TestPreFingerprint_ReadFailureIsNotAgreement(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	err := runOne(t, m, "", errors.New("connection reset"))
	if err == nil {
		t.Fatal("a fingerprint read failure must not pass as agreement")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("refusal should carry the underlying read failure: %v", err)
	}
}

// The header lives on up_post_tx when the in-transaction half is empty — the
// position the console writes for a migration that is entirely a
// non-transactional skirt. Keyed on the marker, not on which segment.
func TestExpectedPreFingerprint_ReadsPostTxSegment(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id:       "ts-1",
		UpPostTx: hdr("cccc3333", "CREATE INDEX CONCURRENTLY i ON x(c);"),
	}
	got, ok := migrate.ExpectedPreFingerprint(m)
	if !ok || got != "cccc3333" {
		t.Errorf("ExpectedPreFingerprint = (%q, %v), want (cccc3333, true)", got, ok)
	}
}

// A data migration's manifest is commented with `#`, and a data migration can
// target a relational connection — so the reader keys on the marker rather
// than on the SQL comment prefix.
func TestExpectedPreFingerprint_AcceptsHashPrefix(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "# wc:expected_pre_fingerprint: dddd4444\nkind: TRANSFORM_FIELD\n",
	}
	got, ok := migrate.ExpectedPreFingerprint(m)
	if !ok || got != "dddd4444" {
		t.Errorf("ExpectedPreFingerprint = (%q, %v), want (dddd4444, true)", got, ok)
	}
}

// --- embedded artifact sets ------------------------------------------

// An embedded set applies exactly like an on-disk one. This is what lets the
// binary that owns the database carry its own migrations: nothing to fetch,
// and nothing writable to fetch INTO — which is the whole point on a pod with
// readOnlyRootFilesystem.
func TestMigrationsFS_AppliesFromAnEmbeddedSet(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	dir := seedDir(t, m)
	stubA := stub.New()
	if err := migrate.Run(context.Background(), migrate.Config{
		Targets:      targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsFS: os.DirFS(dir), // stands in for the go:embed set
		ApplierFor:   func(_ string) (migrate.Applier, error) { return stubA, nil },
	}); err != nil {
		t.Fatalf("embedded set must apply: %v", err)
	}
	if n := len(stubA.Calls()); n != 1 {
		t.Errorf("Apply calls = %d, want 1", n)
	}
}

// The content-hash check has to cover the embedded path too. It is the only
// thing standing between "these bytes are the migration the console signed"
// and "these bytes are whatever ended up in the binary", and an embedded set
// reached by a SECOND loader would be the obvious place for that guarantee to
// quietly not apply.
func TestMigrationsFS_VerifiesContentHash(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	dir := seedDir(t, m)
	// Tamper with the artifact the way an edit after fetch would.
	path := filepath.Join(dir, "main", "ts-1.json")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(buf), "CREATE TABLE x;", "DROP TABLE x;", 1)
	if tampered == string(buf) {
		t.Fatal("fixture did not tamper with anything — the assertion below would prove nothing")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	err = migrate.Run(context.Background(), migrate.Config{
		Targets:      targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsFS: os.DirFS(dir),
		ApplierFor:   func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil {
		t.Fatal("a tampered artifact must be refused when it arrives through MigrationsFS")
	}
	if !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Errorf("refusal should name the hash mismatch: %v", err)
	}
}

// Exactly one source. Both set is a caller bug worth naming rather than a
// silent precedence rule nobody can remember the direction of.
func TestMigrationsFS_RefusesTwoSources(t *testing.T) {
	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", "ts-1", ""}),
		MigrationsDir: t.TempDir(),
		MigrationsFS:  os.DirFS(t.TempDir()),
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one source") {
		t.Fatalf("want a two-sources refusal, got %v", err)
	}
}

// --- in-memory sets (apply --fetch) ----------------------------------

// A fetched set applied straight from memory goes through the SAME loader as
// a disk tree, so the content-hash check covers it. This is the path a deploy
// takes every time; a bypass here would be the one route into the applier
// with no verification on it.
func TestMigrationSetFS_AppliesAndVerifies(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	set, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{m})
	if err != nil {
		t.Fatalf("MigrationSetFS: %v", err)
	}
	stubA := stub.New()
	if err := migrate.Run(context.Background(), migrate.Config{
		Targets:      targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsFS: set,
		ApplierFor:   func(_ string) (migrate.Applier, error) { return stubA, nil },
	}); err != nil {
		t.Fatalf("in-memory set must apply: %v", err)
	}
	if n := len(stubA.Calls()); n != 1 {
		t.Errorf("Apply calls = %d, want 1", n)
	}
}

// A migration whose bytes do not hash to the digest they carry is refused
// even when it never touched a disk — the console-mutation / wire-tampering
// arm, on the route where nobody is watching.
func TestMigrationSetFS_RefusesATamperedBody(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	// Alter the body AFTER the hash was computed, as a mutation in flight
	// would. The digest now describes different bytes.
	m.UpSql = "DROP TABLE x;"
	set, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{m})
	if err != nil {
		t.Fatalf("MigrationSetFS: %v", err)
	}
	err = migrate.Run(context.Background(), migrate.Config{
		Targets:      targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsFS: set,
		ApplierFor:   func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil {
		t.Fatal("a body that does not hash to its own digest must be refused in memory too")
	}
	if !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Errorf("refusal should name the hash mismatch: %v", err)
	}
}

// The in-memory layout must match what WriteMigration puts on disk, or the
// two paths diverge in how they name things and the shared loader stops being
// shared in practice.
func TestMigrationSetFS_UsesTheOnDiskLayout(t *testing.T) {
	m := mkMig("ts-1", "main", "CREATE TABLE x;")
	set, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{m})
	if err != nil {
		t.Fatalf("MigrationSetFS: %v", err)
	}
	if _, err := fs.ReadFile(set, "main/ts-1.json"); err != nil {
		t.Errorf("expected <connection>/<id>.json layout: %v", err)
	}
}

// A migration with no connection cannot be placed in the layout at all, and
// guessing one would put DDL in front of a database nobody named.
func TestMigrationSetFS_RefusesAMigrationWithNoConnection(t *testing.T) {
	_, err := migrate.MigrationSetFS([]*applyfetchpb.Migration{{Id: "ts-1"}})
	if err == nil || !strings.Contains(err.Error(), "empty connection") {
		t.Fatalf("want an empty-connection refusal, got %v", err)
	}
}
