package datamigrate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// R-datamigrate-1: RENAME_FIELD must not silently clobber a
// pre-existing destination field. When both From and To are present
// with DIFFERENT values, the rename would lose the destination value
// — abort instead.

func TestJSONApplyOp_RenameField_ClobberRefused(t *testing.T) {
	raw := []byte(`{"old_name":"src","new_name":"dst"}`)
	_, _, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "old_name", To: "new_name",
	})
	if err == nil {
		t.Fatal("expected error: rename would overwrite an existing destination field")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error should explain the clobber refusal; got: %v", err)
	}
}

// Destination already holds the SAME value → benign already-applied
// state. The source is dropped, stays idempotent (no error).
func TestJSONApplyOp_RenameField_SameValueDestination_Benign(t *testing.T) {
	raw := []byte(`{"old_name":"same","new_name":"same"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "old_name", To: "new_name",
	})
	if err != nil {
		t.Fatalf("same-value destination should be benign: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true (source field removed)")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := doc["old_name"]; present {
		t.Errorf("source field should be removed: %+v", doc)
	}
	if doc["new_name"] != "same" {
		t.Errorf("destination should retain its value: %+v", doc)
	}
}

// Normal rename (destination absent) still works — the guard must not
// regress the happy path.
func TestJSONApplyOp_RenameField_NoDestination_OK(t *testing.T) {
	raw := []byte(`{"old_name":"value"}`)
	out, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "old_name", To: "new_name",
	})
	if err != nil || !changed {
		t.Fatalf("expected clean rename; changed=%v err=%v", changed, err)
	}
	var doc map[string]any
	_ = json.Unmarshal(out, &doc)
	if doc["new_name"] != "value" {
		t.Errorf("new_name = %v", doc["new_name"])
	}
}

// Replay after a successful rename (From gone, To present) is still a
// no-op — the guard only fires when From is ALSO present.
func TestJSONApplyOp_RenameField_ReplayIdempotent(t *testing.T) {
	raw := []byte(`{"new_name":"value"}`)
	_, changed, err := datamigrate.JSONApplyOp(raw, datamigrate.Operation{
		Op: datamigrate.OpRenameField, Keyspace: "x:*",
		From: "old_name", To: "new_name",
	})
	if err != nil {
		t.Fatalf("replay should be a clean no-op: %v", err)
	}
	if changed {
		t.Error("replay on already-renamed value must be a no-op")
	}
}
