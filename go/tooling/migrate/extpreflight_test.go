package migrate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// extFake records every statement it is asked to run, and can be told to fail
// the probe — which is how "nothing was written" gets asserted rather than
// assumed.
type extFake struct {
	ran      []string
	failOn   string // substring; the probe fails when the SQL contains it
	appliedN int
	notPG    bool // answer a non-Postgres dialect
}

// IsPostgres satisfies PostgresDialect. The preflight's probe is Postgres
// syntax, so it only fires against an applier that says it speaks it; these
// tests exercise that path, and `notPG` flips it for the case that must NOT
// be probed (T2-6 pass #10, D10-1).
func (f *extFake) IsPostgres() bool { return !f.notPG }

func (f *extFake) Apply(_ context.Context, m *applyfetchpb.Migration) error {
	f.ran = append(f.ran, m.GetUpSql())
	if m.GetId() != "extension-preflight" {
		f.appliedN++
	}
	if f.failOn != "" && strings.Contains(m.GetUpSql(), f.failOn) {
		return errors.New(`ERROR: required extension vector is not installed in this database`)
	}
	return nil
}
func (f *extFake) Rollback(context.Context, *applyfetchpb.Migration) error { return nil }
func (f *extFake) AppliedHead(context.Context) (string, error)             { return "", nil }
func (f *extFake) Close() error                                            { return nil }

func withManifest(id, conn, json string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{Id: id, Connection: conn, UpSql: "CREATE TABLE t (id BIGINT);", ManifestJson: json}
}

func TestRequiredExtensions_ReadsTheManifest(t *testing.T) {
	m := withManifest("1", "main", `{"required_extensions":["vector","postgis"]}`)
	got := requiredExtensions(m)
	if len(got) != 2 || got[0] != "vector" || got[1] != "postgis" {
		t.Fatalf("got %v", got)
	}
}

// An unparseable or absent manifest yields nothing to check — it must NOT be
// an error. The manifest is metadata attached to a migration, so a field the
// console adds later must not turn an additive change into a refused apply.
func TestRequiredExtensions_UnreadableManifestIsNotAnError(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json at all", `{"required_extensions":"a string"}`} {
		if got := requiredExtensions(withManifest("1", "main", raw)); len(got) != 0 {
			t.Errorf("manifest %q yielded %v, want nothing", raw, got)
		}
	}
}

func TestExtensionPreflightSQL_NamesEachExtensionAndEscapes(t *testing.T) {
	sql := extensionPreflightSQL([]string{"vector", "we'ird"})
	for _, want := range []string{"pg_extension", "extname = 'vector'", "extname = 'we''ird'", "RAISE EXCEPTION"} {
		if !strings.Contains(sql, want) {
			t.Errorf("probe missing %q\n%s", want, sql)
		}
	}
	if extensionPreflightSQL(nil) != "" {
		t.Error("no extensions must produce no probe — an empty DO block is a statement nobody asked to run")
	}
}

// THE test. A missing extension has to stop the run BEFORE any migration is
// applied: the reason this check exists is that failing halfway leaves a
// database neither migrated nor clean.
func TestPreflightExtensions_RefusesBeforeAnythingIsWritten(t *testing.T) {
	fake := &extFake{failOn: "pg_extension"}
	ac := newRunApplierCache(func(string) (Applier, error) { return fake, nil }, io.Discard)

	pending := []Pending{
		{Connection: "main", Migration: withManifest("m1", "main", `{"required_extensions":["vector"]}`)},
		{Connection: "main", Migration: withManifest("m2", "main", "")},
	}

	err := preflightExtensions(context.Background(), ac, pending)
	if err == nil {
		t.Fatal("a missing extension must refuse the run")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("the refusal must name the extension, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Nothing was written") {
		t.Errorf("the refusal must say the database is untouched, got: %v", err)
	}
	if fake.appliedN != 0 {
		t.Fatalf("%d migration(s) were applied despite the refusal — the whole point is that none are", fake.appliedN)
	}
}

func TestPreflightExtensions_SilentWhenNothingIsDeclared(t *testing.T) {
	fake := &extFake{}
	ac := newRunApplierCache(func(string) (Applier, error) { return fake, nil }, io.Discard)
	pending := []Pending{{Connection: "main", Migration: withManifest("m1", "main", "")}}

	if err := preflightExtensions(context.Background(), ac, pending); err != nil {
		t.Fatalf("no declared extensions must not probe at all: %v", err)
	}
	if len(fake.ran) != 0 {
		t.Errorf("probed anyway: %v", fake.ran)
	}
}

// The WIRING test, and the one that matters most.
//
// The tests above exercise preflightExtensions directly, which means they all
// pass with it unhooked from Run — verified by unhooking it, which is exactly
// how a check can exist, be tested, and never run. This one goes through Run,
// so it fails if the call is ever removed.
func TestRun_RefusesWhenAnExtensionIsMissing(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	// Re-digest rather than just setting the field: the manifest's declared
	// extensions are an INPUT to content_sha256, so an artifact carrying a
	// manifest its digest never saw is refused before it is ever read — which
	// is the binding doing its job, and was how this fixture first failed.
	m.ManifestJson = `{"required_extensions":["vector"]}`
	m.ContentSha256 = ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", m.GetManifestJson())
	dir := seedMigs(t, m)
	tg := tgts(tgt("main", m.GetId(), m.GetContentSha256()))
	fake := &extFake{failOn: "pg_extension"}

	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	})
	if err == nil {
		t.Fatal("Run must refuse when a declared extension is absent")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("refusal must name the extension: %v", err)
	}
	if fake.appliedN != 0 {
		t.Fatalf("%d migration(s) applied — Run must write nothing when the preflight refuses", fake.appliedN)
	}
}

// TestPreflightExtensions_DoesNotProbeANonPostgresConnection — the probe is
// Postgres syntax; the manifest field it reads is not Postgres-only.
//
// T2-6 pass #10, D10-1 ≡ B10-4. `plan.go` deliberately flows
// `(w17.pg.field).required_extensions` into non-PG buckets so the manifest
// TRACKS what was declared — the MySQL emitter stamps its own "manifest
// tracking only" marker saying exactly that. The preflight bucketed by
// connection NAME alone and carried no dialect, so a MySQL or SQLite
// connection whose manifest named an extension got a `DO $$ … pg_extension`
// probe fired at it. It fails on syntax, and the wrapper reports "this
// database is missing an extension the schema declares" — a healthy database
// permanently unable to apply, told something untrue about why.
func TestPreflightExtensions_DoesNotProbeANonPostgresConnection(t *testing.T) {
	// An applier that would fail ANY probe: if the preflight fires at all,
	// the run is refused.
	fake := &extFake{failOn: "pg_extension", notPG: true}

	pending := []Pending{{
		Connection: "main",
		Migration: &applyfetchpb.Migration{
			Id:           "ts-1",
			Connection:   "main",
			UpSql:        "CREATE TABLE t (id int primary key);",
			ManifestJson: `{"required_extensions":["pgcrypto"]}`,
		},
	}}

	ac := newRunApplierCache(func(string) (Applier, error) { return fake, nil }, io.Discard)
	defer ac.closeAll()

	if err := preflightExtensions(context.Background(), ac, pending); err != nil {
		t.Fatalf("a non-Postgres connection was probed with Postgres syntax and refused: %v", err)
	}
	for _, sql := range fake.ran {
		if strings.Contains(sql, "pg_extension") {
			t.Errorf("the Postgres probe was sent to a non-Postgres connection:\n%s", sql)
		}
	}

	// Control: the SAME manifest on a Postgres connection must still be
	// probed and still refuse, or this fix would have disabled the check.
	pgFake := &extFake{failOn: "pg_extension"}
	pgAc := newRunApplierCache(func(string) (Applier, error) { return pgFake, nil }, io.Discard)
	defer pgAc.closeAll()
	if err := preflightExtensions(context.Background(), pgAc, pending); err == nil {
		t.Error("a Postgres connection missing a declared extension was NOT refused — the preflight stopped working")
	}
}

// wrappedFake is a decorator of the kind the live harness uses — and the
// kind that a production caller writes without thinking about it. It embeds
// the Applier INTERFACE, so only the interface's own methods are promoted.
type wrappedFake struct {
	Applier
	unwrappable bool
}

func (w wrappedFake) Close() error { return nil }

func (w wrappedFake) Unwrap() Applier {
	if !w.unwrappable {
		// Simulates a decorator written before WrappedApplier existed.
		return nil
	}
	return w.Applier
}

// TestPreflightExtensions_SurvivesADecorator — T2-6 pass #10, D10-1's own
// hole, caught by the live lane.
//
// The gate that stops the Postgres probe reaching a MySQL connection reads
// an OPTIONAL interface off the applier. Embedding a `migrate.Applier`
// interface in a decorator promotes only that interface's methods, so
// `IsPostgres` vanishes the moment anything wraps the applier — and the
// preflight then skips silently against a real Postgres server. A check
// that turns itself off when a decorator appears is the same fail-open the
// gate exists to remove, one layer out.
//
// No unit test over a bare applier could see this; the live dialectdiff
// lane, whose harness wraps with a NoCloseApplier, is what caught it.
func TestPreflightExtensions_SurvivesADecorator(t *testing.T) {
	pending := []Pending{{
		Connection: "main",
		Migration: &applyfetchpb.Migration{
			Id:           "ts-1",
			Connection:   "main",
			UpSql:        "CREATE TABLE t (id int primary key);",
			ManifestJson: `{"required_extensions":["pgcrypto"]}`,
		},
	}}

	inner := &extFake{failOn: "pg_extension"}
	wrapped := wrappedFake{Applier: inner, unwrappable: true}
	ac := newRunApplierCache(func(string) (Applier, error) { return wrapped, nil }, io.Discard)
	defer ac.closeAll()

	if err := preflightExtensions(context.Background(), ac, pending); err == nil {
		t.Fatal("a WRAPPED Postgres applier was not probed — the preflight goes dark behind any decorator, against a real Postgres server")
	}

	// And the non-Postgres answer must still travel through the wrapper, or
	// the fix would have re-opened the hole it closed.
	innerMy := &extFake{failOn: "pg_extension", notPG: true}
	wrappedMy := wrappedFake{Applier: innerMy, unwrappable: true}
	myAc := newRunApplierCache(func(string) (Applier, error) { return wrappedMy, nil }, io.Discard)
	defer myAc.closeAll()
	if err := preflightExtensions(context.Background(), myAc, pending); err != nil {
		t.Errorf("a wrapped non-Postgres connection was probed with Postgres syntax: %v", err)
	}

	// A decorator that does NOT expose its inner applier cannot be seen
	// through, and must fail CLOSED on the skip side — not be treated as
	// Postgres on a guess.
	opaque := wrappedFake{Applier: &extFake{failOn: "pg_extension"}}
	opAc := newRunApplierCache(func(string) (Applier, error) { return opaque, nil }, io.Discard)
	defer opAc.closeAll()
	if err := preflightExtensions(context.Background(), opAc, pending); err != nil {
		t.Errorf("an opaque decorator was probed on a guess: %v", err)
	}
}
