package datamigrate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestJSONApplyOp_PreservesNumberPrecision is the LIB-01 regression
// guard. JSONApplyOp once decoded into map[string]any (numbers →
// float64) then re-marshaled, silently corrupting large ints and
// number representations in fields the op never touches. With
// UseNumber the untouched numeric fields must round-trip verbatim.
func TestJSONApplyOp_PreservesNumberPrecision(t *testing.T) {
	// 9223372036854775807 (max int64) is unrepresentable in float64.
	raw := []byte(`{"id":9223372036854775807,"ratio":1.10,"big":1e6,"obsolete":"x"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "x:*", Field: "obsolete",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := string(out)
	for _, want := range []string{`"id":9223372036854775807`, `"ratio":1.10`, `"big":1e6`} {
		if !strings.Contains(got, want) {
			t.Errorf("untouched numeric field corrupted: want %q in %s", want, got)
		}
	}
	if strings.Contains(got, "obsolete") {
		t.Errorf("op field not removed: %s", got)
	}
}

// TestJSONApplyOp_AddFieldDefault — sets when missing,
// idempotent when present.
func TestJSONApplyOp_AddFieldDefault(t *testing.T) {
	raw := []byte(`{"id":"u1","email":"x@y.z"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "full_name", Value: "anonymous",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on missing field")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if doc["full_name"] != "anonymous" {
		t.Errorf("full_name = %v", doc["full_name"])
	}
	if doc["id"] != "u1" || doc["email"] != "x@y.z" {
		t.Errorf("other fields lost: %+v", doc)
	}

	_, changedAgain, _ := datamigrate.JSONApplyOp(out, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "full_name", Value: "anonymous",
	})
	if changedAgain {
		t.Error("expected changed=false on already-set field")
	}
}

// TestJSONApplyOp_RemoveField — removes when present, no-op
// when missing.
func TestJSONApplyOp_RemoveField(t *testing.T) {
	raw := []byte(`{"id":"u1","obsolete":"x"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "x:*", Field: "obsolete",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if strings.Contains(string(out), "obsolete") {
		t.Errorf("obsolete still present in: %s", out)
	}

	_, changedAgain, _ := datamigrate.JSONApplyOp(out, datamigrate.Operation{
		Op: datamigrate.OpRemoveField, Keyspace: "x:*", Field: "obsolete",
	})
	if changedAgain {
		t.Error("REMOVE re-run on missing field should be no-op")
	}
}

// TestJSONApplyOp_RenameField — copies + clears.
func TestJSONApplyOp_RenameField(t *testing.T) {
	raw := []byte(`{"id":"u1","old_name":"value"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "old_name", To: "new_name",
	})
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	var doc map[string]any
	_ = json.Unmarshal(out, &doc)
	if doc["new_name"] != "value" {
		t.Errorf("new_name = %v", doc["new_name"])
	}
	if _, present := doc["old_name"]; present {
		t.Errorf("old_name still present in: %+v", doc)
	}
}

// TestJSONApplyOp_EmptyValue — zero-byte input short-circuits
// to (nil, false, nil).
func TestJSONApplyOp_EmptyValue(t *testing.T) {
	out, changed, err := datamigrate.JSONApplyOp(nil, datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "x", Value: 1,
	})
	if err != nil || changed || out != nil {
		t.Errorf("expected (nil, false, nil); got out=%v changed=%v err=%v", out, changed, err)
	}
}

// TestJSONApplyOp_MalformedJSON — non-JSON bytes surface an
// explicit decode error so the operator sees it (don't pretend
// to mutate gibberish).
func TestJSONApplyOp_MalformedJSON(t *testing.T) {
	_, _, err := datamigrate.JSONApplyOp([]byte("not-json"), datamigrate.Operation{
		Op: datamigrate.OpAddFieldDefault, Keyspace: "x:*",
		Field: "x", Value: 1,
	})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode JSON") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

// TestJSONApplyOp_UnsupportedOp — anything outside the v1 set
// errors loudly.
func TestJSONApplyOp_UnsupportedOp(t *testing.T) {
	_, _, err := datamigrate.JSONApplyOp([]byte(`{"x":1}`), datamigrate.Operation{
		Op: "BREW_COFFEE", Keyspace: "x:*",
	})
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

// TestDecodeProtoDescriptor_RoundTrip — base64 → bytes
// happy path + empty + malformed.
func TestDecodeProtoDescriptor_RoundTrip(t *testing.T) {
	raw, err := datamigrate.DecodeProtoDescriptor("aGVsbG8=")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw) != "hello" {
		t.Errorf("decoded = %q; want hello", raw)
	}

	if _, err := datamigrate.DecodeProtoDescriptor(""); err == nil {
		t.Error("expected error for empty input")
	}
	if _, err := datamigrate.DecodeProtoDescriptor("not-base64-!!!"); err == nil {
		t.Error("expected error for malformed base64")
	}
}
