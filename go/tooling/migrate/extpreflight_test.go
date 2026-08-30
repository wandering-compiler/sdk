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
}

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
