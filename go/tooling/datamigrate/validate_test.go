package datamigrate_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestValidate_NilMigration — nil pointer surfaces as
// validation error, not a nil-deref panic.
func TestValidate_NilMigration(t *testing.T) {
	if err := datamigrate.Validate(nil); err == nil {
		t.Fatal("expected error for nil migration")
	}
}

// TestValidate_VersionRange — version must be > 0 and ≤
// CurrentVersion.
func TestValidate_VersionRange(t *testing.T) {
	cases := []struct {
		name string
		v    int
		want string
	}{
		{"zero", 0, "version must be"},
		{"negative", -1, "version must be"},
		{"newer-than-supported", datamigrate.CurrentVersion + 1, "unknown to this build"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := datamigrate.Validate(&datamigrate.Migration{
				Version: c.v, Encoding: "json",
				Operations: []datamigrate.Operation{
					{Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y", Value: 1},
				},
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want substring %q", err, c.want)
			}
		})
	}
}

// TestValidate_NegativeParallel — parallel must be ≥ 0
// (zero = fall back to DefaultParallel).
func TestValidate_NegativeParallel(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json", Parallel: -3,
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y", Value: 1},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "parallel must be") {
		t.Errorf("got %v, want parallel-range error", err)
	}
}

// TestValidate_UnknownEncoding — anything outside "json" /
// "protobuf" errors with operator-readable list.
func TestValidate_UnknownEncoding(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "xml",
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y", Value: 1},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("got %v, want unknown-encoding error", err)
	}
}

// TestValidate_JSONWithProtoFields — json encoding must not
// carry proto_descriptor / proto_message.
func TestValidate_JSONWithProtoFields(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		ProtoDescriptor: "deadbeef",
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must not carry") {
		t.Errorf("got %v, want must-not-carry error", err)
	}
}

// TestValidate_MissingKeyspace — every op must declare a
// keyspace.
func TestValidate_MissingKeyspace(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{Op: "ADD_FIELD_DEFAULT", Field: "y"}},
	})
	if err == nil || !strings.Contains(err.Error(), "keyspace is required") {
		t.Errorf("got %v, want keyspace-required error", err)
	}
}

// TestValidate_ScriptOnNonTransform — script / script_lang
// only belong on TRANSFORM_FIELD ops.
func TestValidate_ScriptOnNonTransform(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y",
			Script: "def transform(v): return v", ScriptLang: "starlark",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved for TRANSFORM_FIELD") {
		t.Errorf("got %v, want reserved-for-transform error", err)
	}
}

// TestValidate_TransformFieldWithExtraFields — TRANSFORM_FIELD
// rejects field/from/to/value (the script IS the transform).
func TestValidate_TransformFieldWithExtraFields(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "TRANSFORM_FIELD", Keyspace: "x:*",
			Script: "def transform(v): return v", ScriptLang: "starlark",
			Field: "stray",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "field / from / to / value don't apply") {
		t.Errorf("got %v, want extra-fields error", err)
	}
}

// TestValidate_AddFieldDefaultWithFromTo — ADD_FIELD_DEFAULT
// must not carry from/to (those are RENAME_FIELD's slots).
func TestValidate_AddFieldDefaultWithFromTo(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*", Field: "y", From: "stray",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "ADD_FIELD_DEFAULT") {
		t.Errorf("got %v, want add-field-from-to error", err)
	}
}

// TestValidate_RemoveFieldMissingField — REMOVE without
// field name is malformed.
func TestValidate_RemoveFieldMissingField(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{Op: "REMOVE_FIELD", Keyspace: "x:*"}},
	})
	if err == nil || !strings.Contains(err.Error(), "REMOVE_FIELD") {
		t.Errorf("got %v, want REMOVE_FIELD-required error", err)
	}
}

// TestValidate_RenameFieldEqualFromTo — RENAME with from==to
// is a no-op; refused.
func TestValidate_RenameFieldEqualFromTo(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "RENAME_FIELD", Keyspace: "x:*", From: "a", To: "a",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "from == to") {
		t.Errorf("got %v, want rename-noop error", err)
	}
}

// TestValidate_RenameFieldWithFieldOrValue — RENAME uses
// from/to; field/value reject.
func TestValidate_RenameFieldWithFieldOrValue(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "RENAME_FIELD", Keyspace: "x:*", From: "a", To: "b", Field: "stray",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "use from/to") {
		t.Errorf("got %v, want use-from-to error", err)
	}
}

// TestValidate_TransformWithoutScriptLang — TRANSFORM_FIELD
// must declare script_lang (not just script).
func TestValidate_TransformWithoutScriptLang(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "TRANSFORM_FIELD", Keyspace: "x:*",
			Script: "def transform(v): return v",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "script_lang is required") {
		t.Errorf("got %v, want script_lang-required error", err)
	}
}

// TestValidate_TransformWithUnsupportedLang — only starlark
// in v2.3.
func TestValidate_TransformWithUnsupportedLang(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{
			Op: "TRANSFORM_FIELD", Keyspace: "x:*",
			Script: "x", ScriptLang: "lua",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v, want unsupported-lang error", err)
	}
}

// TestValidate_EmptyOpKind — op-kind missing entirely
// errors before the unknown-op branch.
func TestValidate_EmptyOpKind(t *testing.T) {
	err := datamigrate.Validate(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{{Op: "", Keyspace: "x:*"}},
	})
	if err == nil || !strings.Contains(err.Error(), "op is required") {
		t.Errorf("got %v, want op-required error", err)
	}
}

// TestInverseOperations_NilMigration — defensive nil case.
func TestInverseOperations_NilMigration(t *testing.T) {
	out, irrev := datamigrate.InverseOperations(nil)
	if out != nil || irrev != nil {
		t.Errorf("expected (nil, nil), got out=%v irrev=%v", out, irrev)
	}
}

// TestInverseOperations_FullCoverage — exercises every op-
// kind branch + irreversible recording for REMOVE +
// TRANSFORM. Order is reversed (last applied undoes first).
func TestInverseOperations_FullCoverage(t *testing.T) {
	in := &datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "u:*", Field: "f1", Value: ""},
			{Op: "RENAME_FIELD", Keyspace: "u:*", From: "old", To: "new"},
			{Op: "REMOVE_FIELD", Keyspace: "u:*", Field: "obsolete"},
			{Op: "TRANSFORM_FIELD", Keyspace: "u:*", Script: "x", ScriptLang: "starlark"},
		},
	}
	out, irrev := datamigrate.InverseOperations(in)
	if out == nil {
		t.Fatal("expected non-nil inverse migration")
	}
	// REMOVE + TRANSFORM at original indices 2 + 3 are
	// irreversible. Order in irrev follows reverse-iteration —
	// TRANSFORM (3) seen first.
	if len(irrev) != 2 {
		t.Errorf("expected 2 irreversible indices, got %v", irrev)
	}
	// ADD reverses to REMOVE, RENAME swaps from↔to.
	// Iteration is reverse so TRANSFORM at idx 3 → skipped (irrev),
	// REMOVE at idx 2 → skipped (irrev), RENAME at idx 1 → swap,
	// ADD at idx 0 → REMOVE.
	if len(out.Operations) != 2 {
		t.Fatalf("expected 2 inverse ops, got %d: %+v", len(out.Operations), out.Operations)
	}
	if out.Operations[0].Op != "RENAME_FIELD" || out.Operations[0].From != "new" || out.Operations[0].To != "old" {
		t.Errorf("rename swap wrong: %+v", out.Operations[0])
	}
	if out.Operations[1].Op != "REMOVE_FIELD" || out.Operations[1].Field != "f1" {
		t.Errorf("add→remove inverse wrong: %+v", out.Operations[1])
	}
}
