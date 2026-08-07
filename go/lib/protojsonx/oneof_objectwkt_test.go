package protojsonx_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// TestCollapse_ObjectWKTArmStaysBare pins the documented dialect contract for
// an OBJECT-shaped WKT oneof arm (google.protobuf.Struct): it must collapse to
// a BARE object carrying NO discriminator — the frontend sees the raw struct,
// and ExpandOneofs matches it back as the unique object arm (see the
// isObjectWKTArm / IsDiscriminatorArm doc comments: Struct/Value/ListValue/Any
// "collapse to a bare object/array with no discriminator").
//
// `st` is the Struct arm of the `wktof` oneof in richDesc. A Struct holds
// ARBITRARY user JSON, so injecting w17_discriminator both violates the bare
// contract advertised by the OpenAPI/MCP schema generators (IsDiscriminatorArm
// == false) AND risks clobbering a real user key — see the sibling test.
func TestCollapse_ObjectWKTArmStaysBare(t *testing.T) {
	desc := richDesc(t)
	m := collapse(t, desc, `{"st":{"a":1}}`, nil)
	got, ok := m["wktof"].(map[string]any)
	if !ok {
		t.Fatalf("wktof should collapse to a bare object; got %T: %+v", m["wktof"], m)
	}
	if _, tagged := got[protojsonx.DiscriminatorKey]; tagged {
		t.Errorf("object-WKT (Struct) arm must stay BARE — no %s injected; got %+v",
			protojsonx.DiscriminatorKey, got)
	}
	if got["a"] != float64(1) {
		t.Errorf("bare Struct payload not preserved: %+v", got)
	}
}

// TestCollapse_StructArmPreservesUserDiscriminatorKey is the data-integrity
// half: a Struct is arbitrary user JSON, so it may legitimately contain a key
// literally named "w17_discriminator". Because the Struct arm is bare (no tag
// is injected), that user key must survive collapse verbatim — it must not be
// overwritten with the proto field name.
func TestCollapse_StructArmPreservesUserDiscriminatorKey(t *testing.T) {
	desc := richDesc(t)
	in := `{"st":{"` + protojsonx.DiscriminatorKey + `":"user-value","a":1}}`
	m := collapse(t, desc, in, nil)
	got, ok := m["wktof"].(map[string]any)
	if !ok {
		t.Fatalf("wktof should be a bare object; got %T", m["wktof"])
	}
	if got[protojsonx.DiscriminatorKey] != "user-value" {
		t.Errorf("user's %s key clobbered by collapse: got %v, want %q (data loss)",
			protojsonx.DiscriminatorKey, got[protojsonx.DiscriminatorKey], "user-value")
	}
}

// TestObjectWKT_RoundTrip proves the bare Struct arm still decodes: collapse →
// expand recovers the flat `st` arm via resolveArm's unique-object-WKT branch
// (no discriminator needed). Guards against a "fix" that drops the arm entirely.
func TestObjectWKT_RoundTrip(t *testing.T) {
	desc := richDesc(t)
	collapsed := collapse(t, desc, `{"st":{"a":1}}`, nil)
	// Re-marshal the collapsed map and expand it back.
	bare, ok := collapsed["wktof"].(map[string]any)
	if !ok || bare["a"] != float64(1) {
		t.Fatalf("collapsed wktof not a bare object: %+v", collapsed)
	}
	m := expand(t, desc, `{"wktof":{"a":1}}`, nil)
	st, ok := m["st"].(map[string]any)
	if !ok || st["a"] != float64(1) {
		t.Errorf("bare object-WKT did not expand back to the `st` arm: %+v", m)
	}
}

// TestExpand_Int64StringBareArm covers resolveArm's bare-scalar loop CONTINUING
// past a non-matching arm: the `numbered` oneof is {count int32→number,
// big int64→string, flag bool→boolean}. A JSON string matches `big` (int64
// renders as a JSON string in protojson) — exercising the path where the first
// arm (count, number) is skipped and the second (big, string) matches.
func TestExpand_Int64StringBareArm(t *testing.T) {
	desc := richDesc(t)
	m := expand(t, desc, `{"numbered":"123"}`, nil)
	if m["big"] != "123" {
		t.Errorf("bare string in numbered should match the int64 arm `big`: %+v", m)
	}
	if _, leaked := m["numbered"]; leaked {
		t.Errorf("collapsed oneof key `numbered` should be gone after expand: %+v", m)
	}
}

// TestExpand_NonStringDiscriminator is a hostile-input guard: a collapsed
// object whose w17_discriminator is NOT a string (here a number) must not panic
// and must not resolve to an arm — the `.(string)` assertion fails, no
// object-WKT arm exists for `contact`, so the key passes through unchanged for
// protojson to reject downstream.
func TestExpand_NonStringDiscriminator(t *testing.T) {
	desc := richDesc(t)
	m := expand(t, desc, `{"contact":{"`+protojsonx.DiscriminatorKey+`":123,"x":1}}`, nil)
	c, ok := m["contact"].(map[string]any)
	if !ok {
		t.Fatalf("contact with a non-string discriminator should pass through as-is; got %T", m["contact"])
	}
	if c["x"] != float64(1) {
		t.Errorf("passed-through object payload altered: %+v", c)
	}
}
