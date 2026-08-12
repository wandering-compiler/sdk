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
//   - _time_format=sqlite (T1-4 pass #14, B14-1) — a driver PARAMETER rather
//     than a pragma, and the one entry here that is not spelled `_pragma=`.
//     Without it modernc writes a time.Time with `time.Time.String()`, so the
//     stored text reads `2026-08-11 12:34:56 +0000 UTC` — which `strftime`,
//     `datetime()` and every comparison against `CURRENT_TIMESTAMP` cannot
//     parse. Every temporal predicate then answers NULL and the query matches
//     nothing, silently. This is the SQLite sibling of the MySQL `parseTime`
//     alignment (B-F3): the same question was asked of one driver and never of
//     the other.
//
// Note the name says "pragmas" and the list is DSN settings. Adding the entry
// beat renaming a seam six call sites read; the distinction is stated here
// rather than left for the next reader to discover from a failing parse.
func SQLitePragmas() []string {
	return []string{
		"_pragma=case_sensitive_like(1)",
		"_pragma=foreign_keys(1)",
		"_time_format=sqlite",
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
//   - collation (B-F2) — this pin lived in the DEV DSN only, while
//     go-sql-driver's default handshake collation is utf8mb4_general_ci:
//     a string comparison no column anchors (literal↔literal,
//     param↔literal) was case-SENSITIVE in dev and case-INSENSITIVE on an
//     operator-supplied DSN, silently. `'Foo' = 'foo'` answers 0 under the
//     pin and 1 without it, live on 8.4.10. Column comparisons were never
//     exposed — DDL `COLLATE` anchors those, and the emitter appends
//     `COLLATE` to every character-result cast.
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
		"collation=utf8mb4_0900_as_cs",
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

// PGDSNParams returns the libpq-style DSN parameters every process that
// opens a w17 PostgreSQL database must carry. Both drivers forward
// unrecognised parameters as server startup options — lib/pq through its
// Runtime map, pgx through its config — so one spelling reaches both.
//
//   - standard_conforming_strings (B-F9) — the PG emitter inlines string
//     literals by doubling quotes ONLY, which is correct exactly while
//     this is on. It is settable per database and per role, and with it
//     off the server reads backslashes inside a literal as escapes, so an
//     emitted `'a\b'` stops meaning what the declaration said. Both PG
//     surfaces were exposed: lib/pq has no scs handling at all, and the
//     applier's pgx only checks scs when a query HAS bind arguments,
//     which migration bodies do not.
//   - search_path (B-F15) — w17 puts every table in one schema and
//     addresses it with bare identifiers (the namespace decision chose a
//     prefix over a schema per module), so bare names resolve through
//     search_path. Its default is `"$user", public`, which means an
//     applier running as a role that owns a same-named schema creates
//     `wc_migrations` and the tables somewhere the runtime does not look.
//     Pinning it turns the decision's premise into a fact.
//
// A parameter the operator named themselves is left alone, same rule as
// the MySQL set: these refuse to inherit an ACCIDENTAL setting, they do
// not overrule a chosen one.
func PGDSNParams() []string {
	return []string{
		"standard_conforming_strings=on",
		"search_path=public",
	}
}

// ApplyPGDSNParams appends every parameter from [PGDSNParams] the DSN does
// not already speak to, matching on the parameter NAME so an operator's
// own value survives. Handles both the URL form (`postgres://…?k=v`) and
// the keyword form (`host=… user=…`). Idempotent.
func ApplyPGDSNParams(dsn string) string {
	keyword := !strings.Contains(dsn, "://")
	for _, p := range PGDSNParams() {
		name, value, _ := strings.Cut(p, "=")
		if strings.Contains(dsn, name+"=") {
			continue
		}
		if keyword {
			// libpq keyword/value form: space-separated, and a value with
			// no spaces needs no quoting (both of ours qualify).
			if dsn != "" {
				dsn += " "
			}
			dsn += name + "=" + value
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
