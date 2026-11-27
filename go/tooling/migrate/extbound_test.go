package migrate

import (
	"context"
	"io"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The extension list is read where its digest may not bind it. A digest
// minted before the required-extension segment still loads (ContentHashMatches
// accepts the legacy formula), so an extension list added to such an artifact
// on disk verifies — and the preflight then refused the deploy saying "the
// schema declares" an extension the schema never declared. The preflight still
// runs over such a list (it is a read, and for a genuine legacy artifact an
// early refusal is the safer outcome), but its refusal now says the list is
// not bound by the digest instead of blaming the schema.

func legacyDigestWith(manifest string) *applyfetchpb.Migration {
	m := &applyfetchpb.Migration{Id: "m1", Connection: "main", UpSql: "CREATE TABLE t (id BIGINT);"}
	// Minted with NO manifest: the legacy formula and the current one agree.
	m.ContentSha256 = ContentHash(m.GetUpSql(), "", "", "", "", nil, "", "")
	m.ManifestJson = manifest
	return m
}

func TestExtsBound_LegacyDigestDoesNotBindAnInjectedList(t *testing.T) {
	m := legacyDigestWith(`{"required_extensions":["evil_ext"]}`)
	if !migrationHashMatches(m) {
		t.Fatal("precondition: the legacy digest must still load")
	}
	if extsBound(m) {
		t.Error("extsBound = true for a list the digest never covered")
	}
	// Minted WITH the list: bound.
	bound := &applyfetchpb.Migration{Id: "m1", Connection: "main", UpSql: "CREATE TABLE t (id BIGINT);", ManifestJson: `{"required_extensions":["vector"]}`}
	bound.ContentSha256 = ContentHash(bound.GetUpSql(), "", "", "", "", nil, "", bound.GetManifestJson())
	if !extsBound(bound) {
		t.Error("extsBound = false for a list minted under the extension segment")
	}
	// And with a schema segment beside it.
	both := &applyfetchpb.Migration{Id: "m1", Connection: "main", UpSql: "CREATE TABLE t (id BIGINT);", ManifestJson: `{"required_extensions":["vector"],"required_schemas":["app"]}`}
	both.ContentSha256 = ContentHash(both.GetUpSql(), "", "", "", "", nil, "", both.GetManifestJson())
	if !extsBound(both) {
		t.Error("extsBound = false for extensions minted beside a schema segment")
	}
	// No list: trivially bound.
	if !extsBound(legacyDigestWith("")) {
		t.Error("extsBound = false with no extensions")
	}
}

func TestPreflightExtensions_UnboundListIsAttributed(t *testing.T) {
	fake := &extFake{failOn: "pg_extension"}
	ac := newRunApplierCache(func(string) (Applier, error) { return fake, nil }, io.Discard, 0)
	err := preflightExtensions(context.Background(), ac, []Pending{{Connection: "main", Migration: legacyDigestWith(`{"required_extensions":["evil_ext"]}`)}})
	if err == nil {
		t.Fatal("the preflight must still refuse a missing extension")
	}
	msg := err.Error()
	if strings.Contains(msg, "the schema declares") || !strings.Contains(msg, "does not bind") || !strings.Contains(msg, "m1") {
		t.Errorf("refusal = %q, want it to say migration m1's digest does not bind the list, not that the schema declares it", msg)
	}
	if fake.appliedN != 0 {
		t.Fatalf("%d migration(s) applied despite the refusal", fake.appliedN)
	}

	// Control: a bound list keeps the schema's attribution.
	bound := &applyfetchpb.Migration{Id: "m2", Connection: "main", UpSql: "CREATE TABLE t (id BIGINT);", ManifestJson: `{"required_extensions":["vector"]}`}
	bound.ContentSha256 = ContentHash(bound.GetUpSql(), "", "", "", "", nil, "", bound.GetManifestJson())
	err = preflightExtensions(context.Background(), ac, []Pending{{Connection: "main", Migration: bound}})
	if err == nil || !strings.Contains(err.Error(), "the schema declares") || strings.Contains(err.Error(), "does not bind") {
		t.Errorf("bound refusal = %v, want the schema's attribution", err)
	}
}
