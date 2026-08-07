package dialect

import (
	"errors"
	"regexp"

	"github.com/go-sql-driver/mysql"
)

// MySQL Number → Kind mapping. The error number alone tells
// us which constraint kind fired; the constraint name (or
// column for NOT_NULL) lives inside the message string and
// gets extracted via per-kind regex.
//
// Numbers from MySQL 5.7 / 8.0 / 8.4 (CHECK is 8.0+, before
// that DDL accepted but ignored CHECK clauses):
//
//	1062 → ER_DUP_ENTRY (UNIQUE)
//	1452 → ER_NO_REFERENCED_ROW_2 (FK insert/update)
//	1451 → ER_ROW_IS_REFERENCED_2 (FK delete cascade fail)
//	3819 → ER_CHECK_CONSTRAINT_VIOLATED (CHECK, 8.0+)
//	1048 → ER_BAD_NULL_ERROR (NOT NULL)
//
// Any other number falls through ok=false — Wrap treats as
// internal.
var mysqlKindByNumber = map[uint16]string{
	1062: KindUnique,
	1452: KindFK,
	1451: KindFK,
	3819: KindCheck,
	1048: KindNotNull,
}

// Per-kind extraction regex. Compiled once at package init.
// Patterns are pinned to MySQL's stable engine output:
//
//	1062: "Duplicate entry '<value>' for key '<table>.<key>'"
//	      (5.7-style: just '<key>'; 8.0+ adds <table>. prefix)
//	1452: "... a foreign key constraint fails (`<db>`.`<table>`,
//	       CONSTRAINT `<name>` FOREIGN KEY ...)"
//	1451: same shape as 1452 with `referenced row` phrasing
//	3819: "Check constraint '<name>' is violated."
//	1048: "Column '<column>' cannot be null"
//
// All confirmed empirically against MySQL 8.0 (see
// db-error-classification-portability.md "Empirical
// evidence" section).
var (
	mysqlReUnique  = regexp.MustCompile(`for key '(?:[^'.]+\.)?([^']+)'`)
	mysqlReFK      = regexp.MustCompile("CONSTRAINT `([^`]+)`")
	mysqlReFKTable = regexp.MustCompile("`([^`]+)`\\.`([^`]+)`,\\s*CONSTRAINT")
	mysqlReCheck   = regexp.MustCompile(`Check constraint '([^']+)'`)
	mysqlReNotNull = regexp.MustCompile(`Column '([^']+)'`)
)

// ParseMySQL normalises a MySQL driver error into a
// ConstraintError. The driver returns only Number +
// SQLState + Message strukturálně; constraint name lives
// in Message and gets regex-extracted per error number.
func ParseMySQL(err error) (*ConstraintError, bool) {
	if err == nil {
		return nil, false
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return nil, false
	}
	kind, ok := mysqlKindByNumber[me.Number]
	if !ok {
		return nil, false
	}
	ce := &ConstraintError{Kind: kind}
	switch me.Number {
	case 1062: // UNIQUE
		if m := mysqlReUnique.FindStringSubmatch(me.Message); len(m) == 2 {
			ce.Name = m[1]
		}
	case 1451, 1452: // FK
		if m := mysqlReFK.FindStringSubmatch(me.Message); len(m) == 2 {
			ce.Name = m[1]
		}
		if m := mysqlReFKTable.FindStringSubmatch(me.Message); len(m) == 3 {
			// m[1] = db, m[2] = table; we want just the table for
			// registry lookup (project DB context is global).
			ce.Table = m[2]
		}
	case 3819: // CHECK
		if m := mysqlReCheck.FindStringSubmatch(me.Message); len(m) == 2 {
			ce.Name = m[1]
		}
	case 1048: // NOT_NULL
		if m := mysqlReNotNull.FindStringSubmatch(me.Message); len(m) == 2 {
			ce.Columns = []string{m[1]}
		}
	}
	return ce, true
}
