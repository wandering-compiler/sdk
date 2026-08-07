package datamigrate_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestMarshal_InvalidMigrationRefused — Marshal validates
// before encoding; an invalid Migration surfaces with the
// validation error wrapped, not a half-baked YAML body.
func TestMarshal_InvalidMigrationRefused(t *testing.T) {
	_, err := datamigrate.Marshal(&datamigrate.Migration{
		Version: 1, Encoding: "json",
		// missing operations[]
	})
	if err == nil {
		t.Fatal("expected error for invalid migration")
	}
	if !strings.Contains(err.Error(), "Marshal") || !strings.Contains(err.Error(), "operations") {
		t.Errorf("expected wrapped operations error, got: %v", err)
	}
}

// TestMarshal_NilMigration — nil Migration surfaces via the
// Validate guard, not as a panic.
func TestMarshal_NilMigration(t *testing.T) {
	_, err := datamigrate.Marshal(nil)
	if err == nil {
		t.Fatal("expected error for nil migration")
	}
}

// TestUnmarshal_RejectsMalformedYAML — non-YAML bytes
// surface as decode error.
func TestUnmarshal_RejectsMalformedYAML(t *testing.T) {
	_, err := datamigrate.Unmarshal([]byte("not: : : valid yaml"))
	if err == nil {
		t.Fatal("expected error for malformed yaml")
	}
	if !strings.Contains(err.Error(), "yaml decode") {
		t.Errorf("expected yaml-decode error, got: %v", err)
	}
}
