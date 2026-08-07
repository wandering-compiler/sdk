package dialect

import (
	"errors"
	"testing"
)

// TestParseSQLite_NilAndUnknownCode covers the nil-error short-circuit and the
// "is a *sqlite.Error but not a constraint code" fall-through (e.g. a plain
// SQLITE_ERROR from a bad statement).
func TestParseSQLite_NilAndUnknownCode(t *testing.T) {
	if ce, ok := ParseSQLite(nil); ok || ce != nil {
		t.Errorf("ParseSQLite(nil) = %+v,%v; want nil,false", ce, ok)
	}
	db := openSQLite(t)
	// "no such table" → a *sqlite.Error whose code is not a CONSTRAINT_* code.
	_, err := db.Exec(`INSERT INTO does_not_exist (x) VALUES (1)`)
	if err == nil {
		t.Fatal("expected a sqlite error from a bad statement")
	}
	if ce, ok := ParseSQLite(err); ok || ce != nil {
		t.Errorf("non-constraint sqlite error = %+v,%v; want nil,false", ce, ok)
	}
}

// TestParseMySQL_Nil covers ParseMySQL's nil short-circuit.
func TestParseMySQL_Nil(t *testing.T) {
	if ce, ok := ParseMySQL(nil); ok || ce != nil {
		t.Errorf("ParseMySQL(nil) = %+v,%v; want nil,false", ce, ok)
	}
	// sanity: a non-MySQL error also falls through
	if _, ok := ParseMySQL(errors.New("nope")); ok {
		t.Error("non-mysql error should not classify")
	}
}

// TestPgKind_ExclusionAndDefault covers the 23P01 (exclusion) arm and the
// unknown-SQLSTATE default of pgKind.
func TestPgKind_ExclusionAndDefault(t *testing.T) {
	if k, ok := pgKind("23P01"); !ok || k != KindExclusion {
		t.Errorf("pgKind(23P01) = %q,%v; want exclusion,true", k, ok)
	}
	if k, ok := pgKind("00000"); ok || k != "" {
		t.Errorf("pgKind(unknown) = %q,%v; want \"\",false", k, ok)
	}
}
