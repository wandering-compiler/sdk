package dialect

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

// ParsePg normalises a PG driver error into a
// ConstraintError. Recognises both pgx (`*pgconn.PgError`)
// and lib/pq (`*pq.Error`) — the storage tier uses pgx for
// real connections; lib/pq still appears in some legacy
// paths. Both expose the same set of fields under different
// type names.
//
// Returns (nil, false) when err is not a PG constraint
// violation — caller treats as internal/unrecognised and
// logs the raw error.
//
// SQLSTATE → Kind mapping:
//
//	23505 → unique
//	23503 → fk
//	23514 → check
//	23502 → not_null
//	23P01 → exclusion
//
// Other SQLSTATE classes (40001 serialization, 40P01 deadlock,
// 22001 string truncation, …) are NOT constraint violations
// — they fall through with ok=false. Wrap classifies them
// separately into retryable / internal categories.
func ParsePg(err error) (*ConstraintError, bool) {
	if err == nil {
		return nil, false
	}
	// pgx structured path — preferred for new code.
	var pgxErr *pgconn.PgError
	if errors.As(err, &pgxErr) {
		return pgFromFields(pgxErr.Code, pgxErr.ConstraintName,
			pgxErr.TableName, pgxErr.ColumnName)
	}
	// lib/pq legacy path — same fields, different type.
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pgFromFields(string(pqErr.Code), pqErr.Constraint,
			pqErr.Table, pqErr.Column)
	}
	return nil, false
}

// pgFromFields builds a ConstraintError from the structured
// pieces both PG drivers expose under different type names.
// The four arguments correspond 1:1 to pgconn.PgError /
// pq.Error fields.
func pgFromFields(sqlstate, constraint, table, column string) (*ConstraintError, bool) {
	kind, ok := pgKind(sqlstate)
	if !ok {
		return nil, false
	}
	ce := &ConstraintError{
		Kind:  kind,
		Name:  constraint,
		Table: table,
	}
	if column != "" {
		ce.Columns = []string{column}
	}
	return ce, true
}

func pgKind(sqlstate string) (string, bool) {
	switch sqlstate {
	case "23505":
		return KindUnique, true
	case "23503":
		return KindFK, true
	case "23514":
		return KindCheck, true
	case "23502":
		return KindNotNull, true
	case "23P01":
		return KindExclusion, true
	}
	return "", false
}
