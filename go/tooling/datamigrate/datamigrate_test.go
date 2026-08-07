package datamigrate_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestMarshalUnmarshal_RoundTrip — encode + decode preserves
// every field for the v1 op kinds.
func TestMarshalUnmarshal_RoundTrip(t *testing.T) {
	in := &datamigrate.Migration{
		Version:          1,
		Encoding:         "json",
		Parallel:         8,
		EstimatedRecords: 50000,
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "users:*", Field: "full_name", Value: ""},
			{Op: "REMOVE_FIELD", Keyspace: "sessions:*", Field: "deprecated_token"},
			{Op: "RENAME_FIELD", Keyspace: "events:*", From: "old_name", To: "new_name"},
		},
	}
	body, err := datamigrate.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := datamigrate.Unmarshal(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Version != in.Version || got.Encoding != in.Encoding ||
		got.Parallel != in.Parallel || got.EstimatedRecords != in.EstimatedRecords {
		t.Errorf("scalar fields mismatch:\n  got=%+v\n  want=%+v", got, in)
	}
	if len(got.Operations) != len(in.Operations) {
		t.Fatalf("operations len mismatch: %d vs %d", len(got.Operations), len(in.Operations))
	}
	for i, op := range got.Operations {
		want := in.Operations[i]
		if op.Op != want.Op || op.Keyspace != want.Keyspace ||
			op.Field != want.Field || op.From != want.From || op.To != want.To {
			t.Errorf("op[%d] mismatch:\n  got=%+v\n  want=%+v", i, op, want)
		}
	}
}

// TestUnmarshal_AcceptsProtobufEncoding — encoding=protobuf
// is supported as of v2.2 (D-iter3-19); body must carry both
// proto_descriptor + proto_message.
func TestUnmarshal_AcceptsProtobufEncoding(t *testing.T) {
	body := []byte(`version: 1
encoding: protobuf
proto_descriptor: aGVsbG8=
proto_message: pkg.User
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: x:*
    field: y
    value: 1
`)
	got, err := datamigrate.Unmarshal(body)
	if err != nil {
		t.Fatalf("Unmarshal protobuf encoding: %v", err)
	}
	if got.Encoding != "protobuf" {
		t.Errorf("Encoding = %q, want protobuf", got.Encoding)
	}
	if got.ProtoMessage != "pkg.User" {
		t.Errorf("ProtoMessage = %q, want pkg.User", got.ProtoMessage)
	}
}

// TestUnmarshal_RejectsProtobufWithoutDescriptor — encoding=
// protobuf needs both proto_descriptor + proto_message;
// missing either is a validation refusal.
func TestUnmarshal_RejectsProtobufWithoutDescriptor(t *testing.T) {
	body := []byte(`version: 1
encoding: protobuf
proto_message: pkg.User
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: x:*
    field: y
    value: 1
`)
	_, err := datamigrate.Unmarshal(body)
	if err == nil {
		t.Fatal("expected error for protobuf without proto_descriptor")
	}
	if !strings.Contains(err.Error(), "proto_descriptor") {
		t.Errorf("expected proto_descriptor error, got: %v", err)
	}
}

// TestUnmarshal_RejectsProtobufWithoutMessage — counterpart
// for the missing-message field.
func TestUnmarshal_RejectsProtobufWithoutMessage(t *testing.T) {
	body := []byte(`version: 1
encoding: protobuf
proto_descriptor: aGVsbG8=
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: x:*
    field: y
    value: 1
`)
	_, err := datamigrate.Unmarshal(body)
	if err == nil {
		t.Fatal("expected error for protobuf without proto_message")
	}
	if !strings.Contains(err.Error(), "proto_message") {
		t.Errorf("expected proto_message error, got: %v", err)
	}
}

// TestUnmarshal_AcceptsTransformField — TRANSFORM_FIELD op
// kind graduated to v2.3 (D-iter3-20). Body with a Starlark
// script + matching script_lang must round-trip.
func TestUnmarshal_AcceptsTransformField(t *testing.T) {
	body := []byte(`version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: x:*
    script_lang: starlark
    script: |
      def transform(value):
          return value
`)
	got, err := datamigrate.Unmarshal(body)
	if err != nil {
		t.Fatalf("Unmarshal TRANSFORM_FIELD: %v", err)
	}
	if got.Operations[0].Op != "TRANSFORM_FIELD" {
		t.Errorf("Op = %q, want TRANSFORM_FIELD", got.Operations[0].Op)
	}
	if got.Operations[0].ScriptLang != "starlark" {
		t.Errorf("ScriptLang = %q, want starlark", got.Operations[0].ScriptLang)
	}
}

// TestUnmarshal_RejectsTransformWithoutScript — script body
// is required when op=TRANSFORM_FIELD.
func TestUnmarshal_RejectsTransformWithoutScript(t *testing.T) {
	body := []byte(`version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: x:*
    script_lang: starlark
`)
	_, err := datamigrate.Unmarshal(body)
	if err == nil {
		t.Fatal("expected error for missing script")
	}
	if !strings.Contains(err.Error(), "script is required") {
		t.Errorf("expected script-required error, got: %v", err)
	}
}

// TestUnmarshal_RejectsTransformUnknownLang — only starlark
// is supported in v2.3.
func TestUnmarshal_RejectsTransformUnknownLang(t *testing.T) {
	body := []byte(`version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: x:*
    script_lang: lua
    script: |
      function transform(v) return v end
`)
	_, err := datamigrate.Unmarshal(body)
	if err == nil {
		t.Fatal("expected error for script_lang=lua")
	}
	if !strings.Contains(err.Error(), "starlark") {
		t.Errorf("expected starlark-only error, got: %v", err)
	}
}

// TestValidate_RejectsMisshapenOps — per-kind invariants.
func TestValidate_RejectsMisshapenOps(t *testing.T) {
	cases := map[string]*datamigrate.Migration{
		"ADD without field": {
			Version: 1, Encoding: "json",
			Operations: []datamigrate.Operation{{Op: "ADD_FIELD_DEFAULT", Keyspace: "x:*"}},
		},
		"REMOVE with from/to": {
			Version: 1, Encoding: "json",
			Operations: []datamigrate.Operation{{Op: "REMOVE_FIELD", Keyspace: "x:*", Field: "y", From: "a"}},
		},
		"RENAME without from": {
			Version: 1, Encoding: "json",
			Operations: []datamigrate.Operation{{Op: "RENAME_FIELD", Keyspace: "x:*", To: "b"}},
		},
		"RENAME from==to": {
			Version: 1, Encoding: "json",
			Operations: []datamigrate.Operation{{Op: "RENAME_FIELD", Keyspace: "x:*", From: "a", To: "a"}},
		},
		"unknown op": {
			Version: 1, Encoding: "json",
			Operations: []datamigrate.Operation{{Op: "BREW_COFFEE", Keyspace: "x:*"}},
		},
		"empty operations": {
			Version: 1, Encoding: "json", Operations: nil,
		},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := datamigrate.Validate(m); err == nil {
				t.Errorf("expected validation error for %q", name)
			}
		})
	}
}

// TestLooksLikeYAML_Classifier — content-shape sniff.
func TestLooksLikeYAML_Classifier(t *testing.T) {
	yes := []string{
		"version: 1\nencoding: json\n",
		"# wc:expected_pre_fingerprint: abc\nversion: 1\n",
		"# wc:risk: ...\n\n# more comments\nversion: 1\n",
	}
	no := []string{
		"BEGIN;\nCREATE TABLE x;\nCOMMIT;",
		"HSET wc:migrations ts hex",
		"nats kv put wc-migrations ts hex",
		"S3 PUT wc-migrations/ts.json {json}",
		"",
		"# only comments\n# nothing else",
	}
	for _, body := range yes {
		if !datamigrate.LooksLikeYAML([]byte(body)) {
			t.Errorf("expected YAML classifier=true for: %q", body)
		}
	}
	for _, body := range no {
		if datamigrate.LooksLikeYAML([]byte(body)) {
			t.Errorf("expected YAML classifier=false for: %q", body)
		}
	}
}

// TestEffectiveParallel — CLI override > YAML default >
// fallback constant.
func TestEffectiveParallel(t *testing.T) {
	m := &datamigrate.Migration{Parallel: 8}
	if got := datamigrate.EffectiveParallel(m, 16); got != 16 {
		t.Errorf("CLI override: got %d, want 16", got)
	}
	if got := datamigrate.EffectiveParallel(m, 0); got != 8 {
		t.Errorf("YAML default: got %d, want 8", got)
	}
	if got := datamigrate.EffectiveParallel(&datamigrate.Migration{}, 0); got != datamigrate.DefaultParallel {
		t.Errorf("fallback: got %d, want %d", got, datamigrate.DefaultParallel)
	}
	if got := datamigrate.EffectiveParallel(nil, 0); got != datamigrate.DefaultParallel {
		t.Errorf("nil migration fallback: got %d, want %d", got, datamigrate.DefaultParallel)
	}
}

// TestInverseOperations_AutoDerived — forward → down mapping.
func TestInverseOperations_AutoDerived(t *testing.T) {
	fwd := &datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{
			{Op: "ADD_FIELD_DEFAULT", Keyspace: "users:*", Field: "full_name", Value: ""},
			{Op: "RENAME_FIELD", Keyspace: "events:*", From: "old", To: "new"},
		},
	}
	inv, irreversible := datamigrate.InverseOperations(fwd)
	if len(irreversible) != 0 {
		t.Errorf("no irreversible ops expected, got indices %v", irreversible)
	}
	if len(inv.Operations) != 2 {
		t.Fatalf("expected 2 inverse ops, got %d", len(inv.Operations))
	}
	// Order is reversed — RENAME first then REMOVE.
	if inv.Operations[0].Op != "RENAME_FIELD" ||
		inv.Operations[0].From != "new" || inv.Operations[0].To != "old" {
		t.Errorf("rename inverse wrong: %+v", inv.Operations[0])
	}
	if inv.Operations[1].Op != "REMOVE_FIELD" ||
		inv.Operations[1].Field != "full_name" {
		t.Errorf("add-default inverse wrong: %+v", inv.Operations[1])
	}
}

// TestInverseOperations_RemoveFieldIrreversible — REMOVE_FIELD
// in forward direction has no inverse; surfaces in the
// `irreversible` slice.
func TestInverseOperations_RemoveFieldIrreversible(t *testing.T) {
	fwd := &datamigrate.Migration{
		Version: 1, Encoding: "json",
		Operations: []datamigrate.Operation{
			{Op: "REMOVE_FIELD", Keyspace: "users:*", Field: "deprecated"},
		},
	}
	inv, irreversible := datamigrate.InverseOperations(fwd)
	if len(irreversible) != 1 || irreversible[0] != 0 {
		t.Errorf("expected irreversible=[0], got %v", irreversible)
	}
	if len(inv.Operations) != 0 {
		t.Errorf("expected zero inverse ops, got %d", len(inv.Operations))
	}
}
