package dialect

import (
	"errors"
	"fmt"
	"testing"
)

// lookalike has the method set of modernc.org/sqlite's *Error and is not it.
type lookalike struct{}

func (*lookalike) Error() string { return "UNIQUE constraint failed: u.email" }
func (*lookalike) Code() int     { return 2067 }

// AsSQLite finds the driver's *Error through a %w wrap and an errors.Join,
// the way errors.As did when this package imported the driver, and takes
// nothing else for it — the match is by package and type name, not by shape.
func TestAsSQLite_FindsTheDriverErrorOnly(t *testing.T) {
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE u (id INTEGER PRIMARY KEY, email TEXT, CONSTRAINT u_email_unique UNIQUE (email))`)
	mustExec(t, db, `INSERT INTO u (email) VALUES ('a@b.c')`)
	_, raw := db.Exec(`INSERT INTO u (email) VALUES ('a@b.c')`)
	if raw == nil {
		t.Fatal("expected a UNIQUE violation")
	}
	for name, err := range map[string]error{
		"bare":    raw,
		"wrapped": fmt.Errorf("insert: %w", raw),
		"joined":  errors.Join(errors.New("first"), fmt.Errorf("second: %w", raw)),
	} {
		se, ok := AsSQLite(err)
		if !ok || se.Code() != 2067 {
			t.Errorf("%s: AsSQLite = %v, %v", name, se, ok)
		}
	}
	for name, err := range map[string]error{
		"lookalike": fmt.Errorf("x: %w", &lookalike{}),
		"plain":     errors.New("UNIQUE constraint failed: u.email"),
		"nil":       nil,
	} {
		if se, ok := AsSQLite(err); ok {
			t.Errorf("%s: taken for a SQLite error: %v", name, se)
		}
	}
}
