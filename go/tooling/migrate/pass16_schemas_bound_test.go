package migrate

import (
	"context"
	"io"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// T2-5 pass #16 (R16-6, A16-5). `required_schemas` decides what executes at a
// production apply — prepareSchemas runs CREATE SCHEMA for every name in it —
// and it rode OUTSIDE the content digest: three manifests that differed only
// in it hashed to one digest, and an artifact whose list was edited on disk
// passed the loader. The coverage gate's excuse for manifest_json ("the one
// part the client ACTS on — required_extensions — IS hashed") went stale the
// day schema prep became a second consumer.
//
// The marker is a benign, attacker-named EMPTY namespace: what the measured
// edit could do.

// minted is m as the console stores it: its digest computed over its content.
func minted(m *applyfetchpb.Migration) *applyfetchpb.Migration {
	m.ContentSha256 = ContentHashWithChecks(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(),
		m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), m.GetAdoptPreflightSql(), m.GetManifestJson(), m.GetChecks())
	return m
}

func TestContentHash_BindsTheSchemaSet(t *testing.T) {
	digest := func(manifest string) string {
		return ContentHash("CREATE TABLE \"billing\".\"invoice\" (id BIGINT);", "", "", "", "", nil, "", manifest)
	}
	base := digest(`{"required_schemas":["billing"]}`)
	if base == digest(`{"required_schemas":["billing","zz_marker_ns"]}`) {
		t.Error("adding a schema leaves the digest unchanged — a fetched artifact could make the apply create a namespace nobody planned")
	}
	if base == digest(`{"required_schemas":["ledger"]}`) {
		t.Error("swapping the schema leaves the digest unchanged")
	}
	if base == digest(`{}`) {
		t.Error("stripping required_schemas leaves the digest unchanged")
	}
	// The set is the semantics, as for extensions.
	if base != digest(`{"required_schemas":["billing","billing"," billing "]}`) {
		t.Error("a duplicated entry moved the digest")
	}
	if digest(`{"required_schemas":["a","b"]}`) != digest(`{"required_schemas":["b","a"]}`) {
		t.Error("reordering moved the digest")
	}
	// A migration with no schemas keeps the digest it always had.
	if digest(`{}`) != ContentHash("CREATE TABLE \"billing\".\"invoice\" (id BIGINT);", "", "", "", "", nil, "", "") {
		t.Error("a manifest declaring no schema moved the digest — every stored pin would break")
	}
}

// An artifact minted WITH a schema list, edited on disk: the loader refuses it.
func TestSchemasEdited_OnAMintedArtifact_FailTheDigest(t *testing.T) {
	m := minted(withManifest("m1", "app", `{"required_schemas":["billing"]}`))
	for _, edited := range []string{
		`{"required_schemas":["billing","zz_marker_ns"]}`,
		`{"required_schemas":[]}`,
		`{}`,
	} {
		e := minted(withManifest("m1", "app", `{"required_schemas":["billing"]}`))
		e.ManifestJson = edited
		if migrationHashMatches(e) {
			t.Errorf("manifest edited to %s still matches the digest %s", edited, m.GetContentSha256())
		}
	}
}

// What the loader must keep accepting for compatibility — a digest minted
// before the schema segment existed — never licenses CREATE SCHEMA: schema
// prep acts only on schemas the digest binds. So an artifact minted with no
// schemas, edited to declare one, creates nothing.
func TestPrepareSchemas_CreatesOnlyTheSchemasTheDigestBinds(t *testing.T) {
	pg := &extFake{}
	ac := newRunApplierCache(func(string) (Applier, error) { return pg, nil }, io.Discard, 0)

	bound := minted(withManifest("m1", "app", `{"required_schemas":["billing"]}`))
	added := minted(withManifest("m2", "app", ""))
	added.ManifestJson = `{"required_schemas":["zz_marker_ns"]}`

	if !migrationHashMatches(added) {
		t.Fatalf("precondition: a legacy-formula digest must still load (compatibility)")
	}
	pending := []Pending{{Connection: "app", Migration: bound}, {Connection: "app", Migration: added}}
	if err := prepareSchemas(context.Background(), ac, pending); err != nil {
		t.Fatal(err)
	}
	ran := strings.Join(pg.ran, "\n")
	if strings.Contains(ran, "zz_marker_ns") {
		t.Errorf("schema prep created a namespace the digest does not bind:\n%s", ran)
	}
	if !strings.Contains(ran, `CREATE SCHEMA "billing"`) {
		t.Errorf("schema prep dropped the bound schema:\n%s", ran)
	}
}

// The Wiper is handed only bound namespaces too.
func TestDeclaredNamespaces_OnlyBoundOnes(t *testing.T) {
	bound := minted(withManifest("m1", "app", `{"required_schemas":["billing"]}`))
	added := minted(withManifest("m2", "app", ""))
	added.ManifestJson = `{"required_schemas":["zz_marker_ns"]}`
	got := DeclaredNamespaces([]*applyfetchpb.Migration{bound, added}, "app")
	if len(got) != 1 || got[0] != "billing" {
		t.Errorf("got %v, want [billing]", got)
	}
}
