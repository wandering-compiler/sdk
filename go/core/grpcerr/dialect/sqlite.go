package dialect

import (
	"errors"
	"regexp"
	"strings"

	sqlitelib "modernc.org/sqlite"
)

// SQLite extended result codes (`sqlite3_extended_errcode`).
// The driver's `Error.Code()` returns these directly. We
// classify off the extended code, then regex-extract whatever
// identification info the message string carries — varies
// by kind:
//
//	2067 SQLITE_CONSTRAINT_UNIQUE     → columns only (no constraint name)
//	787  SQLITE_CONSTRAINT_FOREIGNKEY → NOTHING — degraded case (see
//	                                    decision doc)
//	275  SQLITE_CONSTRAINT_CHECK      → constraint name (when named)
//	1299 SQLITE_CONSTRAINT_NOTNULL    → table.column
//	2579 SQLITE_CONSTRAINT_PRIMARYKEY → columns only — treated as UNIQUE
//	1555 SQLITE_CONSTRAINT_ROWID      → none — treated as UNIQUE on ROWID
//
// Other extended codes under SQLITE_CONSTRAINT (TRIGGER,
// FUNCTION, VTAB, COMMITHOOK, FOREIGNTABLE) fall through
// ok=false — those aren't user-data violations.
var sqliteKindByCode = map[int]string{
	2067: KindUnique,  // CONSTRAINT_UNIQUE
	2579: KindUnique,  // CONSTRAINT_PRIMARYKEY (treat as UNIQUE)
	1555: KindUnique,  // CONSTRAINT_ROWID
	787:  KindFK,      // CONSTRAINT_FOREIGNKEY (degraded — no info)
	275:  KindCheck,   // CONSTRAINT_CHECK
	1299: KindNotNull, // CONSTRAINT_NOTNULL
}

var (
	// "UNIQUE constraint failed: probe_users.email"
	// Multi-column composite: "probe_pairs.a, probe_pairs.b"
	sqliteReUnique = regexp.MustCompile(`UNIQUE constraint failed:\s*(.+?)(?:\s*\(\d+\))?$`)
	// "CHECK constraint failed: <name>"
	sqliteReCheck = regexp.MustCompile(`CHECK constraint failed:\s*(.+?)(?:\s*\(\d+\))?$`)
	// "NOT NULL constraint failed: probe_users.email"
	sqliteReNotNull = regexp.MustCompile(`NOT NULL constraint failed:\s*(.+?)(?:\s*\(\d+\))?$`)
)

// ParseSQLite normalises a modernc.org/sqlite driver error
// into a ConstraintError. SQLite is the most degraded
// dialect — see db-error-classification-portability.md for
// the FK-attribution problem.
//
// The runtime layer combines this with the registry's
// sole-FK heuristic + the codegen-emitted multi-FK warning
// to surface a reasonable user message in the common case
// (single-FK tables).
func ParseSQLite(err error) (*ConstraintError, bool) {
	if err == nil {
		return nil, false
	}
	var se *sqlitelib.Error
	if !errors.As(err, &se) {
		return nil, false
	}
	kind, ok := sqliteKindByCode[se.Code()]
	if !ok {
		return nil, false
	}
	ce := &ConstraintError{Kind: kind}
	msg := se.Error()
	switch kind {
	case KindUnique:
		if m := sqliteReUnique.FindStringSubmatch(msg); len(m) == 2 {
			// Either "table.col" or "t.c, t.c, …" for composite.
			ce.Table, ce.Columns = parseSQLiteColumnList(m[1])
		}
	case KindCheck:
		if m := sqliteReCheck.FindStringSubmatch(msg); len(m) == 2 {
			ce.Name = strings.TrimSpace(m[1])
		}
	case KindNotNull:
		if m := sqliteReNotNull.FindStringSubmatch(msg); len(m) == 2 {
			ce.Table, ce.Columns = parseSQLiteColumnList(m[1])
		}
	case KindFK:
		// Nothing in the message — Wrap falls back to the
		// registry's sole-FK-per-table heuristic.
	}
	return ce, true
}

// parseSQLiteColumnList parses a SQLite column-list string
// into (table, columns). Inputs vary:
//
//	"probe_users.email"            → ("probe_users", ["email"])
//	"probe_pairs.a, probe_pairs.b" → ("probe_pairs", ["a", "b"])
//	"foo, bar"                     → ("",            ["foo", "bar"])
//
// All composite UNIQUE rows in SQLite share one table prefix
// (no cross-table UNIQUE). When the prefix is consistent
// across all entries, we return it; mixed/missing → empty
// table + bare names. Registry lookup tolerates either.
func parseSQLiteColumnList(s string) (table string, cols []string) {
	parts := strings.Split(s, ",")
	// ambiguous latches once any entry is bare (no `table.` prefix) or
	// carries a prefix different from the first — at that point we can no
	// longer attribute the list to one table, and a later same-looking
	// prefix must NOT resurrect the table. (Q55-grpcerr-2: the bare-column
	// case previously left an earlier prefix in place, mis-attributing a
	// mixed list to the first column's table.)
	ambiguous := false
	for i, p := range parts {
		p = strings.TrimSpace(p)
		dot := strings.LastIndex(p, ".")
		if dot <= 0 {
			cols = append(cols, p) // bare column — no usable table prefix
			table, ambiguous = "", true
			continue
		}
		t, c := p[:dot], p[dot+1:]
		cols = append(cols, c)
		switch {
		case ambiguous:
			// stay ambiguous; table already cleared
		case i == 0:
			table = t
		case t != table:
			table, ambiguous = "", true
		}
	}
	return table, cols
}
