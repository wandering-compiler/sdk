package datamigrate_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// A stored value that is not one JSON object is refused with an error rather
// than mutated: an array, a scalar, or an object followed by more data would
// otherwise be rewritten as something it never was — the trailing data
// silently dropped.
func TestJSONApplyOp_NotAJSONObjectIsRefused(t *testing.T) {
	op := datamigrate.Operation{Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*", Field: "f", Value: 1}
	for _, raw := range []string{
		`[1,2]`,
		`"text"`,
		`42`,
		`true`,
		`{"a":1}{"b":2}`,
		`{"a":1} trailing`,
	} {
		out, changed, err := datamigrate.JSONApplyOp([]byte(raw), op)
		if err == nil {
			t.Errorf("%s: accepted (changed=%v, out=%s), want an error", raw, changed, out)
		}
	}
}

// A data migration body is exactly one YAML document: the operations of a
// second one would never be validated or run while the migration recorded
// itself as applied.
func TestUnmarshal_MoreThanOneDocumentIsRefused(t *testing.T) {
	body := "version: 1\nencoding: json\noperations:\n  - op: REMOVE_FIELD\n    keyspace: users:*\n    field: a\n" +
		"---\nversion: 1\nencoding: json\noperations:\n  - op: REMOVE_FIELD\n    keyspace: users:*\n    field: b\n"
	if _, err := datamigrate.Unmarshal([]byte(body)); err == nil {
		t.Fatal("a body with two YAML documents was accepted")
	}
	if _, err := datamigrate.Unmarshal([]byte("---\n" + body[:strings.Index(body, "---")])); err != nil {
		t.Fatalf("one document behind a leading ---: %v", err)
	}
}
