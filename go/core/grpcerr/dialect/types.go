// Package dialect normalises database driver errors into a
// dialect-agnostic ConstraintError so the cross-dialect Wrap
// path (`lib/grpcerr.Wrap`) can look up author-supplied
// validation_messages by either constraint name or
// (table, kind, columns) tuple — see
// `docs/decisions/db-error-classification-portability.md`
// for the full rationale and empirical evidence.
//
// One adapter per dialect (`ParsePg`, `ParseMySQL`,
// `ParseSQLite`, future `ParseMSSQL`, `ParseOracle`); each
// returns the same shape regardless of how rich the source
// driver's error type happens to be.
//
// PG exposes constraint name as a structured field; MySQL /
// SQLite / MSSQL / Oracle require regex extraction from the
// message string. Patterns are stable across DB-engine
// versions (the engine emits them, not the driver), so the
// regex layer is empirically safe — but tested per-driver
// against real engine output to catch any future regression.
package dialect

// Kind classifies a constraint violation. The five values
// cover every relational constraint type we generate plus
// EXCLUSION (PG-only today, future Oracle-with-extension).
const (
	KindUnique    = "unique"
	KindFK        = "fk"
	KindCheck     = "check"
	KindNotNull   = "not_null"
	KindExclusion = "exclusion"
)

// ConstraintError is the dialect-normalised representation of
// a constraint violation. Per-dialect adapters fill the
// fields they can extract; the registry lookup layer
// (lib/grpcerr.ConstraintRegistry) consults them in
// precedence order: Name → (Table + Kind + Columns) → sole-
// instance heuristic for FK.
//
// Empty fields are normal — different dialects expose
// different subsets:
//
//   - PG: all fields filled via pgconn/pq structured fields.
//   - MySQL: Name + Kind for UNIQUE/FK/CHECK; Columns for
//     NOT_NULL; Table when in CONSTRAINT clause.
//   - SQLite: Name + Kind for CHECK; Table + Columns + Kind
//     for UNIQUE/NOT_NULL; only Kind for FK (the degraded
//     case — see decision doc).
//   - MSSQL / Oracle: Name + Kind for all four; Columns +
//     Table when message includes them.
type ConstraintError struct {
	// Kind is one of the Kind* constants above. Always
	// populated when the parser returns ok.
	Kind string

	// Name is the constraint identifier as the engine
	// reported it. Empty when the dialect doesn't expose it
	// (notably SQLite UNIQUE/FK/NOT_NULL).
	Name string

	// Table is the table whose constraint fired. Empty when
	// the dialect doesn't include it in the message (rare —
	// even SQLite includes the table for UNIQUE/NOT_NULL
	// since it formats as `<table>.<col>`).
	Table string

	// Columns is the column list the violation pertains to,
	// in declaration order. Always set for NOT_NULL (single
	// column); set for UNIQUE on dialects that expose
	// columns instead of the constraint name (SQLite). Nil
	// when the dialect provides no column information.
	Columns []string
}
