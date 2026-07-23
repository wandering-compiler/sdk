// Package sqlitecollate registers the Unicode collation the wandering-compiler
// emits on SQLite string columns (`COLLATE W17_UNICODE`) so SQLite orders and
// compares text like the canonical PostgreSQL dialect (and the aligned MySQL
// utf8mb4_0900_as_cs) instead of BINARY byte order (F7-A-5 / WOB3).
//
// SQLite's built-in BINARY collation sorts by raw byte value, so every
// uppercase letter precedes every lowercase one ('Z' < 'a') and accented
// letters land after 'z' — a `ORDER BY name` that reads [Adam, Zoe, adam] where
// PostgreSQL reads [adam, Adam, Zoe]. The W17_UNICODE collation is the Unicode
// Collation Algorithm root order (accent- and case-SENSITIVE): text sorts by
// base letter first with accents and case as tiebreaks, and equality stays
// accent+case sensitive ('Foo' != 'foo'), mirroring MySQL as_cs.
//
// SQLite resolves a column's `COLLATE <name>` at CREATE TABLE time, so the SAME
// collation must be registered in BOTH the migration applier (which runs the
// DDL and builds the indexes) and the generated service binary (which runs the
// queries). Registering it from one shared package guarantees the ordering an
// index was built with is byte-for-byte the ordering a query compares with — a
// mismatch would silently corrupt index-backed lookups and ORDER BY.
package sqlitecollate

import (
	"database/sql/driver"
	"strings"
	"sync"

	moderncsqlite "modernc.org/sqlite"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Name is the SQLite collation name the compiler emits on string columns.
// The migrator's DDL emitter references the same string (guarded by a drift
// test) so `CREATE TABLE ... COLLATE W17_UNICODE` and this registration agree.
const Name = "W17_UNICODE"

// collate.Collator is stateful (it carries internal comparison iterators), so
// it is NOT safe for concurrent use, yet the collation callback runs on every
// string comparison across every connection. A pool of collators keeps the hot
// path lock-free: each comparison borrows a collator for the duration of the
// call and returns it.
var collators = sync.Pool{New: func() any { return collate.New(language.Und) }}

// Compare is the collation function: the Unicode Collation Algorithm root
// order, accent- and case-sensitive. Exported so callers (and tests) can reuse
// exactly the ordering the registered SQLite collation applies.
func Compare(a, b string) int {
	c := collators.Get().(*collate.Collator)
	defer collators.Put(c)
	return c.CompareString(a, b)
}

// FoldCase applies fold to a single SQLite argument, matching the builtin's
// type handling: NULL stays NULL and non-text values pass through unchanged
// (modernc hands TEXT as a Go string, everything else as its Go type).
// Exported so tests can exercise the exact logic the registered UDFs run.
func FoldCase(args []driver.Value, fold func(string) string) driver.Value {
	if len(args) != 1 || args[0] == nil {
		return nil
	}
	if s, ok := args[0].(string); ok {
		return fold(s)
	}
	return args[0]
}

func unicodeUpper(_ *moderncsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	return FoldCase(args, strings.ToUpper), nil
}

func unicodeLower(_ *moderncsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	return FoldCase(args, strings.ToLower), nil
}

var once sync.Once

// Register installs the W17_UNICODE collation AND Unicode-aware upper()/lower()
// on every modernc.org/sqlite connection opened after the call, so SQLite's
// text ordering, case-folding and case-insensitive comparisons all match the
// canonical PostgreSQL dialect (F7-A-5 collation + F7-A-4 upper/lower). SQLite's
// built-in upper()/lower() only fold ASCII, so upper('café')='CAFé' where PG
// yields 'CAFÉ' — the registered UDFs override the builtins with Go's Unicode
// simple case mapping (strings.ToUpper/ToLower). Deterministic (same input →
// same output, usable under an expression index).
//
// It is idempotent (modernc rejects a second registration of the same name, so
// a sync.Once guards it) and safe to call from multiple init paths — the
// migration applier, the dev-DB snapshotter, and every generated storage binary
// all call it before opening a connection, so an expression index / generated
// column / query folds text identically whichever process computes it (F8-D-4).
func Register() {
	once.Do(func() {
		moderncsqlite.MustRegisterCollationUtf8(Name, Compare)
		moderncsqlite.MustRegisterDeterministicScalarFunction("upper", 1, unicodeUpper)
		moderncsqlite.MustRegisterDeterministicScalarFunction("lower", 1, unicodeLower)
	})
}
