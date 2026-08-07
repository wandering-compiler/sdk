// Package dbsession declares the per-dialect SESSION SETTINGS that the
// SQL this compiler emits depends on for its meaning.
//
// It exists because those settings were scattered. T1-4 pass #12 found
// the family had ~9 members hand-written across FOUR implementations
// with four different coverage sets — the dev DSN builder, the generated
// binary's runtime rewrite, the migration applier's DSN builders, and
// the dialectdiff harness — and every one carried a different subset.
// Each disagreement was its own defect:
//
//   - the SQLite runtime never pinned foreign_keys, so the ON DELETE
//     actions the migrator emits were inert at serving time (B-F1);
//   - the SQLite applier never pinned case_sensitive_like, so a LIKE in
//     a CHECK or a partial index meant one thing at apply and another at
//     runtime (B-F7);
//   - the MySQL applier never pinned sql_mode, so the backslash-doubling
//     the DDL emitter performs corrupted apply-time literals under
//     NO_BACKSLASH_ESCAPES (B-F6);
//   - parseTime rode the dev DSN only, so an operator-supplied MySQL DSN
//     broke every timestamp scan (B-F3).
//
// The shape is the one this audit keeps meeting: a setting that exists
// only as a comment, or only in the one builder whose author needed it.
// So the answer is not another helper — it is ONE declaration every
// process reads, in the PUBLIC sdk because the applier lives here and
// cannot import the private compiler core. Same reasoning that put
// sqlitecollate here: applier, dev-DB snapshotter and every generated
// runtime have to agree byte for byte.
//
// Adding a setting is a list entry. That is the point — the next one
// should not depend on anybody remembering these call sites exist.
package dbsession

import "strings"

// SQLitePragmas returns the modernc.org/sqlite DSN pragmas every process
// that opens a w17 SQLite database must carry.
//
//   - case_sensitive_like(1) (F15-3-5) — LIKE matches PostgreSQL's
//     case-sensitive semantics rather than SQLite's ASCII-insensitive
//     default, so one DQL `LIKE` means one thing on every dialect.
//   - foreign_keys(1) (B-F1) — modernc defaults it OFF; without it the
//     `ON DELETE CASCADE | RESTRICT | SET NULL` actions the migrator
//     emits are declared but never enforced.
func SQLitePragmas() []string {
	return []string{
		"_pragma=case_sensitive_like(1)",
		"_pragma=foreign_keys(1)",
	}
}

// ApplySQLitePragmas appends every pragma from [SQLitePragmas] that the
// DSN does not already carry. Idempotent; handles a DSN with or without
// an existing query string.
func ApplySQLitePragmas(dsn string) string {
	for _, p := range SQLitePragmas() {
		if strings.Contains(dsn, p) {
			continue
		}
		if strings.Contains(dsn, "?") {
			dsn += "&" + p
			continue
		}
		dsn += "?" + p
	}
	return dsn
}

// MySQLDSNParams returns the go-sql-driver DSN parameters every process
// that opens a w17 MySQL database must carry.
//
//   - sql_mode (D-F11) — an EXPRESSION the server evaluates, so it clears
//     the one flag the emitter cannot tolerate and leaves every other
//     operator choice intact. The emitter escapes MySQL string literals
//     for the default sql_mode: under NO_BACKSLASH_ESCAPES its doubling
//     stops being a no-op and `'a\\b'` becomes the four-character `a\b`.
//   - parseTime (B-F3) — generated code scans DATETIME straight into
//     time.Time, which the driver refuses without it.
//
// A parameter the DSN already names is left alone: these are safety pins
// for an ACCIDENTAL setting, not a way to overrule a chosen one. The
// distinction is worth stating next to the list, because a pin that
// overrides a deliberate configuration is a different and worse thing
// than one that supplies a missing default.
func MySQLDSNParams() []string {
	return []string{
		"sql_mode=REPLACE(@@sql_mode,'NO_BACKSLASH_ESCAPES','')",
		"parseTime=true",
	}
}

// ApplyMySQLDSNParams appends every parameter from [MySQLDSNParams] the
// DSN does not already speak to, matching on the parameter NAME so an
// operator's own value survives. Idempotent.
func ApplyMySQLDSNParams(dsn string) string {
	for _, p := range MySQLDSNParams() {
		name, _, _ := strings.Cut(p, "=")
		if strings.Contains(dsn, name+"=") {
			continue
		}
		if strings.Contains(dsn, "?") {
			dsn += "&" + p
			continue
		}
		dsn += "?" + p
	}
	return dsn
}
