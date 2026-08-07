package redis

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestBuildCodec_JSONEncodingReturnsNil pins that a JSON-encoded
// migration needs no protobuf codec — buildCodec short-circuits
// to (nil, nil) so the per-key path uses the JSON helper.
func TestBuildCodec_JSONEncodingReturnsNil(t *testing.T) {
	for _, enc := range []string{"", datamigrate.EncodingJSON} {
		codec, err := buildCodec(&datamigrate.Migration{Encoding: enc})
		if err != nil {
			t.Fatalf("buildCodec(enc=%q): unexpected err %v", enc, err)
		}
		if codec != nil {
			t.Errorf("buildCodec(enc=%q) = %v, want nil codec", enc, codec)
		}
	}
}

// TestBuildCodec_ProtobufBadDescriptorBase64 pins that an
// un-decodable proto_descriptor is a migration-refusal — the
// base64 decode error surfaces before any KV mutation.
func TestBuildCodec_ProtobufBadDescriptorBase64(t *testing.T) {
	_, err := buildCodec(&datamigrate.Migration{
		Encoding:        datamigrate.EncodingProtobuf,
		ProtoDescriptor: "!!!not-base64!!!",
		ProtoMessage:    "pkg.User",
	})
	if err == nil {
		t.Fatal("expected base64 decode error, got nil")
	}
}

// TestBuildCodec_ProtobufInvalidDescriptorBytes pins that
// well-formed base64 whose bytes are not a FileDescriptorSet is
// rejected by NewProtoCodec.
func TestBuildCodec_ProtobufInvalidDescriptorBytes(t *testing.T) {
	// "aGVsbG8=" decodes to "hello", not a valid FDS.
	_, err := buildCodec(&datamigrate.Migration{
		Encoding:        datamigrate.EncodingProtobuf,
		ProtoDescriptor: "aGVsbG8=",
		ProtoMessage:    "pkg.User",
	})
	if err == nil {
		t.Fatal("expected proto codec error for non-FDS bytes, got nil")
	}
}

// TestBuildTransformVMs_NoTransformOpsReturnsNil pins that a
// migration with no TRANSFORM_FIELD op compiles to a nil map —
// the worker pool then never consults the VM table.
func TestBuildTransformVMs_NoTransformOpsReturnsNil(t *testing.T) {
	vms, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{
			{Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*", Field: "y"},
			{Op: datamigrate.OpRemoveField, Keyspace: "x:*", Field: "z"},
		},
	})
	if err != nil {
		t.Fatalf("buildTransformVMs: %v", err)
	}
	if vms != nil {
		t.Errorf("expected nil VM map, got %v", vms)
	}
}

// TestBuildTransformVMs_CompilesGoodScript pins that a valid
// Starlark transform compiles up-front and is keyed by op index.
func TestBuildTransformVMs_CompilesGoodScript(t *testing.T) {
	vms, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{
			{Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*", Field: "y"},
			{
				Op:         datamigrate.OpTransformField,
				Keyspace:   "x:*",
				ScriptLang: "starlark",
				Script:     "def transform(value):\n    return value\n",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildTransformVMs: %v", err)
	}
	if vms == nil || vms[1] == nil {
		t.Fatalf("expected VM at op index 1, got %v", vms)
	}
	if _, ok := vms[0]; ok {
		t.Errorf("non-transform op 0 should not have a VM")
	}
}

// TestBuildTransformVMs_BadScriptAborts pins that a script
// without a top-level transform() function aborts the migration
// at compile time (op index reported in the error).
func TestBuildTransformVMs_BadScriptAborts(t *testing.T) {
	_, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{
			{
				Op:         datamigrate.OpTransformField,
				Keyspace:   "x:*",
				ScriptLang: "starlark",
				Script:     "x = 1\n",
			},
		},
	})
	if err == nil {
		t.Fatal("expected transform compile error, got nil")
	}
	if !strings.Contains(err.Error(), "op[0]") {
		t.Errorf("error should name the failing op index, got %v", err)
	}
}

// TestIsIrreversibleMarkerBody pins the comment-block detector
// that distinguishes "rollback refused" from "rollback applies a
// YAML body".
func TestIsIrreversibleMarkerBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"whitespace-only", "   \n\t\n", false},
		{"pure marker", "# wc:irreversible: REMOVE_FIELD has no inverse", true},
		{"marker among blank+comment lines", "\n# header\n\n# wc:irreversible: x\n", true},
		{"comment without marker", "# just a note\n# another", false},
		{"real yaml body is not a marker", "version: 1\noperations: []", false},
		{"comment then real line", "# note\nversion: 1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIrreversibleMarkerBody(tc.body); got != tc.want {
				t.Errorf("isIrreversibleMarkerBody(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestParseDSN_Empty pins the explicit empty-DSN guard ahead of
// go-redis's own URL parsing.
func TestParseDSN_Empty(t *testing.T) {
	if _, err := ParseDSN(""); err == nil {
		t.Error("empty DSN should be refused")
	}
}

// TestClose_NilClientIsNoOp pins the nil-client guard in Close
// (unreachable through New, which always builds a client).
func TestClose_NilClientIsNoOp(t *testing.T) {
	a := &Applier{}
	if err := a.Close(); err != nil {
		t.Errorf("Close on nil client = %v, want nil", err)
	}
}

// TestValidate_RedisConfig pins the empty-corner guard on a
// parsed Config: nil Options or empty DSN is refused.
func TestValidate_RedisConfig(t *testing.T) {
	if err := Validate(Config{}); err == nil {
		t.Error("empty Config should be refused")
	}
	ok, err := ParseDSN("redis://localhost:6379/0")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if err := Validate(ok); err != nil {
		t.Errorf("valid Config refused: %v", err)
	}
}
