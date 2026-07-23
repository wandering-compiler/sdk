package dialect

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

// PG fixtures recorded 2026-05-09 against PostgreSQL 18 in
// docker (wc-schemas-pg18). See db-error-classification-
// portability.md "Empirical evidence" for the SQL that
// generated each row. Constructing pgconn.PgError directly
// here matches what the driver produces over the wire.

func TestParsePg_Pgx_Unique(t *testing.T) {
	err := &pgconn.PgError{
		Code:           "23505",
		Message:        `duplicate key value violates unique constraint "probe_users_email_unique"`,
		Detail:         `Key (email)=(alice@example.com) already exists.`,
		SchemaName:     "public",
		TableName:      "probe_users",
		ConstraintName: "probe_users_email_unique",
	}
	ce, ok := ParsePg(err)
	if !ok {
		t.Fatal("ok=false for known UNIQUE")
	}
	if ce.Kind != KindUnique {
		t.Errorf("Kind = %q, want %q", ce.Kind, KindUnique)
	}
	if ce.Name != "probe_users_email_unique" {
		t.Errorf("Name = %q", ce.Name)
	}
	if ce.Table != "probe_users" {
		t.Errorf("Table = %q", ce.Table)
	}
}

func TestParsePg_Pgx_FK(t *testing.T) {
	err := &pgconn.PgError{
		Code:           "23503",
		ConstraintName: "probe_orders_user_fk",
		TableName:      "probe_orders",
	}
	ce, ok := ParsePg(err)
	if !ok || ce.Kind != KindFK {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "probe_orders_user_fk" {
		t.Errorf("Name = %q", ce.Name)
	}
}

func TestParsePg_Pgx_Check(t *testing.T) {
	err := &pgconn.PgError{
		Code:           "23514",
		ConstraintName: "probe_users_age_check",
		TableName:      "probe_users",
	}
	ce, ok := ParsePg(err)
	if !ok || ce.Kind != KindCheck {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
}

func TestParsePg_Pgx_NotNull(t *testing.T) {
	err := &pgconn.PgError{
		Code:       "23502",
		TableName:  "probe_users",
		ColumnName: "email",
	}
	ce, ok := ParsePg(err)
	if !ok || ce.Kind != KindNotNull {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if len(ce.Columns) != 1 || ce.Columns[0] != "email" {
		t.Errorf("Columns = %v", ce.Columns)
	}
}

func TestParsePg_LibPq_Unique(t *testing.T) {
	err := &pq.Error{
		Code:       "23505",
		Constraint: "probe_users_email_unique",
		Table:      "probe_users",
	}
	ce, ok := ParsePg(err)
	if !ok || ce.Name != "probe_users_email_unique" {
		t.Fatalf("lib/pq path broke: %+v ok=%v", ce, ok)
	}
}

func TestParsePg_NonConstraintSQLState(t *testing.T) {
	// 40001 serialization_failure is NOT a constraint
	// violation — Wrap classifies it elsewhere as retryable.
	err := &pgconn.PgError{Code: "40001"}
	if _, ok := ParsePg(err); ok {
		t.Error("40001 should fall through ok=false")
	}
}

func TestParsePg_NotPgError(t *testing.T) {
	if _, ok := ParsePg(errors.New("network failure")); ok {
		t.Error("plain error should fall through ok=false")
	}
}

func TestParsePg_NilError(t *testing.T) {
	if _, ok := ParsePg(nil); ok {
		t.Error("nil should fall through ok=false")
	}
}
