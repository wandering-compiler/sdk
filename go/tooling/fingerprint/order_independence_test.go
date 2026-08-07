package fingerprint_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
)

// TestFingerprintHex_IndependentOfColumnAndTableOrder is the canonicalisation
// invariant that makes the fingerprint a trustworthy drift signal: two schemas
// describing the SAME tables + columns must hash identically no matter what
// order the extractor returned them in. DB catalog queries don't guarantee a
// stable row order across engine versions / replicas, so without this Format()
// sorts both tables AND columns — but the existing SortsMultipleTables test
// uses one column per table, so deleting the column sort would still pass it
// while silently making the fingerprint order-sensitive (→ phantom migration
// drift on every redeploy). This pins the column-order leg explicitly.
func TestFingerprintHex_IndependentOfColumnAndTableOrder(t *testing.T) {
	colsAB := []fingerprint.Column{
		{Name: "alpha", DataType: "TEXT", Nullable: true},
		{Name: "beta", DataType: "INT", Default: "0"},
		{Name: "gamma", DataType: "BOOL"},
	}
	colsReversed := []fingerprint.Column{
		{Name: "gamma", DataType: "BOOL"},
		{Name: "beta", DataType: "INT", Default: "0"},
		{Name: "alpha", DataType: "TEXT", Nullable: true},
	}

	// Same two tables, both table order AND column order permuted.
	canonical := fingerprint.Schema{Tables: []fingerprint.Table{
		{Name: "accounts", Columns: colsAB},
		{Name: "orders", Columns: colsAB},
	}}
	permuted := fingerprint.Schema{Tables: []fingerprint.Table{
		{Name: "orders", Columns: colsReversed},
		{Name: "accounts", Columns: colsReversed},
	}}

	if a, b := canonical.FingerprintHex(), permuted.FingerprintHex(); a != b {
		t.Errorf("fingerprint is order-sensitive:\n canonical = %s\n permuted  = %s\n canonical Format:\n%s\n permuted Format:\n%s",
			a, b, canonical.Format(), permuted.Format())
	}
}

// TestFingerprintHex_DistinguishesColumnChange is the paired sensitivity guard:
// order independence must not be achieved by throwing away information. A real
// schema difference (a column's nullability flips) MUST change the hash, or the
// fingerprint would mask genuine drift.
func TestFingerprintHex_DistinguishesColumnChange(t *testing.T) {
	base := fingerprint.Schema{Tables: []fingerprint.Table{
		{Name: "users", Columns: []fingerprint.Column{{Name: "email", DataType: "TEXT", Nullable: false}}},
	}}
	changed := fingerprint.Schema{Tables: []fingerprint.Table{
		{Name: "users", Columns: []fingerprint.Column{{Name: "email", DataType: "TEXT", Nullable: true}}},
	}}
	if base.FingerprintHex() == changed.FingerprintHex() {
		t.Error("nullability change did not alter the fingerprint — genuine drift would be masked")
	}
}
