package mysql

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// TestIsMissingTable pins the missing-table predicate: it matches
// any of the three go-sql-driver "table doesn't exist" shapes
// (numeric code 1146, the English message, the "Unknown table"
// variant), survives wrapping, and rejects unrelated / nil errors.
func TestIsMissingTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"error 1146", errors.New("Error 1146 (42S02): Table 'x.wc_migrations' doesn't exist"), true},
		{"doesn't exist phrase", errors.New("table doesn't exist"), true},
		{"unknown table", errors.New("Unknown table 'x.wc_migrations'"), true},
		{"wrapped 1146", fmt.Errorf("query: %w", errors.New("Error 1146: nope")), true},
		{"unrelated", errors.New("connection refused"), false},
		{"syntax error", errors.New("Error 1064 (42000): You have an error in your SQL syntax"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMissingTable(tt.err); got != tt.want {
				t.Errorf("isMissingTable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestClose_NilDBIsNoop pins the idempotency guard: Close on an
// Applier whose db pool is already nil returns nil and never panics.
// Constructed directly because New always opens a live pool.
func TestClose_NilDBIsNoop(t *testing.T) {
	a := &Applier{}
	if err := a.Close(); err != nil {
		t.Errorf("Close on nil-db Applier = %v, want nil", err)
	}
}

// TestClose_RealPoolClosesAndNils covers the non-nil branch without a live
// server: sql.Open is lazy (it never dials), so we get a real *sql.DB whose
// Close() succeeds offline. Close must return nil AND nil out the pool so a
// second Close is the no-op guard above (idempotent shutdown).
func TestClose_RealPoolClosesAndNils(t *testing.T) {
	db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/db?multiStatements=true")
	if err != nil {
		t.Fatalf("sql.Open (lazy, should not dial): %v", err)
	}
	a := &Applier{db: db}
	if err := a.Close(); err != nil {
		t.Errorf("Close on live pool = %v, want nil", err)
	}
	if a.db != nil {
		t.Error("Close must nil out the pool so a second Close is a no-op")
	}
	// Idempotent: the now-nil Applier closes again without panic.
	if err := a.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
