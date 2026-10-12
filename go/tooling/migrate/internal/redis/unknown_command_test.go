package redis_test

import (
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A command line the store does not recognise fails the migration — it is not
// skipped — and the migration is not recorded as applied.
func TestApply_UnknownCommandFailsTheMigration_Live(t *testing.T) {
	a, mr := liveApplier(t)
	err := a.Apply(liveCtx(t), &applyfetchpb.Migration{
		Id:    "20260401T000009Z",
		UpSql: "SET a 1\nFROBNICATE a\nSET b 2",
	})
	if err == nil || !strings.Contains(err.Error(), "FROBNICATE") {
		t.Fatalf("an unrecognised command line did not fail the migration: %v", err)
	}
	if mr.Exists("b") {
		t.Error("the line after the unrecognised one ran")
	}
	if mr.Exists("w17:migrations") && mr.HGet("w17:migrations", "20260401T000009Z") != "" {
		t.Error("a migration that failed on an unrecognised line was recorded as applied")
	}
}
