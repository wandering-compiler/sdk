package typere_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/typere"
)

// T2-6 pass #8, B-F5 — `(w17.field) = {type: DECIMAL, precision: 10,
// scale: 2}` promised a refusal ("Refused when … the fee carries more digits
// than the column may hold") that nothing enforced. A DDL CHECK cannot
// enforce it: PostgreSQL coerces the literal to the column type BEFORE
// evaluating constraints, so `'19.905'` is already `19.91` by the time a
// `CHECK (scale(col) <= 2)` runs (live-proven on postgres:18). The request
// side is the only place the promise can hold, and FitsDecimal is the
// predicate the generated validation block calls.
//
// The rule is VALUE-PRESERVATION, not digit-counting: padding zeros carry no
// information, so `19.900` fits scale 2 (PG stores 19.90 losslessly) while
// `19.905` does not (it would be silently rounded).
func TestFitsDecimal(t *testing.T) {
	cases := []struct {
		s     string
		p, sc int32
		want  bool
	}{
		// The empty string belongs to the required / blank rules — every
		// other format check in the emitter guards with `!= ""` too.
		{"", 10, 2, true},
		// Exactly representable.
		{"19.90", 10, 2, true},
		{"19.9", 10, 2, true},
		{"19", 10, 2, true},
		{"-19.90", 10, 2, true},
		{"+19.90", 10, 2, true},
		{".5", 10, 2, true},
		{"5.", 10, 2, true},
		// Padding zeros are lossless — refusing them would reject values the
		// column stores exactly.
		{"19.900", 10, 2, true},
		{"19.9000000", 10, 2, true},
		{"0019.90", 10, 2, true},
		{"-0019.900", 10, 2, true},
		{"0.00", 10, 2, true},
		// The live-proven defect: a third SIGNIFICANT fractional digit is
		// rounded away by PG, silently altering a money value.
		{"19.905", 10, 2, false},
		{"19.901", 10, 2, false},
		{"-19.905", 10, 2, false},
		// Integer digits: precision − scale is the ceiling. 10,2 → 8 digits.
		{"99999999.99", 10, 2, true},
		{"123456789.99", 10, 2, false},
		{"00123456789", 10, 2, false},
		// scale 0 — an integer-like DECIMAL.
		{"1234", 4, 0, true},
		{"12345", 4, 0, false},
		{"12.0", 4, 0, true},
		{"12.5", 4, 0, false},
		// precision == scale — only values below 1.
		{"0.55", 2, 2, true},
		{"1.55", 2, 2, false},
		// Not a decimal at all. A DECIMAL carrier is a string on the wire, so
		// nothing else refuses garbage before it reaches the driver.
		{"not a number", 10, 2, false},
		{"19.9.5", 10, 2, false},
		{"1e3", 10, 2, false},
		{"19,90", 10, 2, false},
		{"-", 10, 2, false},
		{".", 10, 2, false},
		{" 19.90", 10, 2, false},
		{"19.90 ", 10, 2, false},
		{"NaN", 10, 2, false},
		// Undeclared precision — nothing to enforce; the IR builder already
		// requires `precision` for DECIMAL, so this is the defensive arm.
		{"19.905", 0, 0, true},
	}
	for _, tc := range cases {
		if got := typere.FitsDecimal(tc.s, tc.p, tc.sc); got != tc.want {
			t.Errorf("FitsDecimal(%q, %d, %d) = %v; want %v", tc.s, tc.p, tc.sc, got, tc.want)
		}
	}
}
