// Package dbsession declares the per-dialect SESSION SETTINGS that the
// SQL this compiler emits depends on for its meaning.
//
// It exists because those settings were scattered. The family had ~9 members hand-written across FOUR implementations
// with four different coverage sets — the dev DSN builder, the generated
// binary's runtime rewrite, the migration applier's DSN builders, and
// the dialectdiff harness — and every one carried a different subset.
// Each disagreement was its own defect:
//
//   - the SQLite runtime never pinned foreign_keys, so the ON DELETE
//     actions the migrator emits were inert at serving time;
//   - the SQLite applier never pinned case_sensitive_like, so a LIKE in
//     a CHECK or a partial index meant one thing at apply and another at
//     runtime;
//   - the MySQL applier never pinned sql_mode, so the backslash-doubling
//     the DDL emitter performs corrupted apply-time literals under
//     NO_BACKSLASH_ESCAPES;
//   - parseTime rode the dev DSN only, so an operator-supplied MySQL DSN
//     broke every timestamp scan.
//
// The shape is a recurring one: a setting that exists
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

import (
	"log/slog"
	"net/url"
	"strings"
	"sync"
)

// SQLitePragmas returns the modernc.org/sqlite DSN pragmas every process
// that opens a w17 SQLite database must carry.
//
//   - case_sensitive_like(1) — LIKE matches PostgreSQL's
//     case-sensitive semantics rather than SQLite's ASCII-insensitive
//     default, so one DQL `LIKE` means one thing on every dialect.
//   - foreign_keys(1) — modernc defaults it OFF; without it the
//     `ON DELETE CASCADE | RESTRICT | SET NULL` actions the migrator
//     emits are declared but never enforced.
//   - _time_format=sqlite — a driver PARAMETER rather
//     than a pragma, and the one entry here that is not spelled `_pragma=`.
//     Without it modernc writes a time.Time with `time.Time.String()`, so the
//     stored text reads `2026-08-11 12:34:56 +0000 UTC` — which `strftime`,
//     `datetime()` and every comparison against `CURRENT_TIMESTAMP` cannot
//     parse. Every temporal predicate then answers NULL and the query matches
//     nothing, silently. This is the SQLite sibling of the MySQL `parseTime`
//     alignment: the same question was asked of one driver and never of
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
// DSN does not already speak to. Idempotent; handles a DSN with or without
// an existing query string.
//
// A setting the operator named is kept, by NAME, exactly as on the other
// two dialects: `_pragma=foreign_keys(0)` stays and `foreign_keys(1)` is
// not appended after it. Matching the exact pin text instead appended the
// pin next to the operator's own value, and the driver applies pragmas in
// sorted order, so the pin silently won. A kept setting that differs from
// the pin is logged (the parameter, never the DSN), because it changes
// what the emitted SQL means.
func ApplySQLitePragmas(dsn string) string {
	named := sqliteSettings(dsn)
	for _, p := range SQLitePragmas() {
		name, pinned := sqliteSetting(p)
		if have, ok := named[name]; ok {
			if have != pinned {
				reportKept("sqlite", name, pinned)
			}
			continue
		}
		dsn = appendQuery(dsn, strings.IndexByte(dsn, '?') >= 0, p)
	}
	return dsn
}

// sqliteSettings returns the settings a modernc DSN names, keyed the way
// [sqliteSetting] keys a pin. The driver splits the DSN at its first `?`
// and URL-decodes the rest, so this does the same.
func sqliteSettings(dsn string) map[string]string {
	out := map[string]string{}
	i := strings.IndexByte(dsn, '?')
	if i < 0 {
		return out
	}
	for _, kv := range strings.Split(dsn[i+1:], "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		name, value := sqliteSetting(unescapeQuery(k) + "=" + unescapeQuery(v))
		out[name] = value
	}
	return out
}

// sqliteSetting splits one `key=value` DSN entry into the name it sets and
// a normalised value. A `_pragma` entry is keyed by the pragma it runs —
// `foreign_keys(0)`, `foreign_keys=0` and `main.foreign_keys = off` all set
// `pragma:foreign_keys` to `0` — every other entry by its parameter name.
func sqliteSetting(entry string) (name, value string) {
	k, v, _ := strings.Cut(entry, "=")
	if k != "_pragma" {
		return k, v
	}
	v = strings.ToLower(strings.TrimSpace(v))
	end := strings.IndexAny(v, "(= ")
	pragma, arg := v, ""
	if end >= 0 {
		pragma, arg = v[:end], v[end:]
	}
	if dot := strings.LastIndexByte(pragma, '.'); dot >= 0 {
		pragma = pragma[dot+1:]
	}
	arg = strings.Trim(arg, "()= \t")
	switch arg {
	case "on", "true", "yes":
		arg = "1"
	case "off", "false", "no":
		arg = "0"
	}
	return "pragma:" + pragma, arg
}

// MySQLDSNParams returns the go-sql-driver DSN parameters every process
// that opens a w17 MySQL database must carry.
//
//   - sql_mode — an EXPRESSION the server evaluates, so it clears
//     the one flag the emitter cannot tolerate and leaves every other
//     operator choice intact. The emitter escapes MySQL string literals
//     for the default sql_mode: under NO_BACKSLASH_ESCAPES its doubling
//     stops being a no-op and `'a\\b'` becomes the four-character `a\b`.
//   - parseTime — generated code scans DATETIME straight into
//     time.Time, which the driver refuses without it.
//   - collation — this pin lived in the DEV DSN only, while
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
//
// The name is read from the DSN's parameter list — what follows the `?`
// after the database name, the way go-sql-driver parses it — not searched
// for anywhere in the string: a password containing `collation=` or a `?`
// is part of the credentials, and must neither suppress a pin nor decide
// where the next one goes.
func ApplyMySQLDSNParams(dsn string) string {
	named, hasQuery := mysqlParams(dsn)
	for _, p := range MySQLDSNParams() {
		name, pinned, _ := strings.Cut(p, "=")
		if have, ok := named[name]; ok {
			if have != pinned {
				reportKept("mysql", name, pinned)
			}
			continue
		}
		dsn = appendQuery(dsn, hasQuery, p)
		hasQuery = true
	}
	return dsn
}

// mysqlParams returns the parameters a go-sql-driver DSN names and whether
// it has a parameter list at all. The driver takes the LAST `/` as the start
// of the database name (credentials and address come before it, and may
// contain anything), and the parameters follow the first `?` after it.
func mysqlParams(dsn string) (map[string]string, bool) {
	out := map[string]string{}
	rest := dsn
	if slash := strings.LastIndexByte(dsn, '/'); slash >= 0 {
		rest = dsn[slash+1:]
	}
	q := strings.IndexByte(rest, '?')
	if q < 0 {
		return out, false
	}
	for _, kv := range strings.Split(rest[q+1:], "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		out[k] = unescapeQuery(v)
	}
	return out, true
}

// PGDSNParams returns the libpq-style DSN parameters every process that
// opens a w17 PostgreSQL database must carry. Both drivers forward
// unrecognised parameters as server startup options — lib/pq through its
// Runtime map, pgx through its config — so one spelling reaches both.
//
//   - standard_conforming_strings — the PG emitter inlines string
//     literals by doubling quotes ONLY, which is correct exactly while
//     this is on. It is settable per database and per role, and with it
//     off the server reads backslashes inside a literal as escapes, so an
//     emitted `'a\b'` stops meaning what the declaration said. Both PG
//     surfaces were exposed: lib/pq has no scs handling at all, and the
//     applier's pgx only checks scs when a query HAS bind arguments,
//     which migration bodies do not.
//   - search_path — w17 puts every table in one schema and
//     addresses it with bare identifiers (the namespace decision chose a
//     prefix over a schema per module), so bare names resolve through
//     search_path. Its default is `"$user", public`, which means an
//     applier running as a role that owns a same-named schema creates
//     `w17_migrations` and the tables somewhere the runtime does not look.
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
//
// The DSN is parsed, not searched: a password that contains
// `search_path=` does not suppress the pin, and a parameter the operator set
// through `options` (`options=-c search_path=app`, URL-encoded or not) counts
// as named — appending a second `search_path` startup parameter next to it
// left the effective value to driver ordering. A DSN that does not parse is
// returned unchanged; the driver reports it.
func ApplyPGDSNParams(dsn string) string {
	if strings.Contains(dsn, "://") {
		return applyPGURL(dsn)
	}
	named, ok := pgKeywords(dsn)
	if !ok {
		return dsn
	}
	for _, p := range PGDSNParams() {
		name, value, _ := strings.Cut(p, "=")
		if have, ok := named[name]; ok {
			if have != value {
				reportKept("postgres", name, value)
			}
			continue
		}
		// libpq keyword/value form: space-separated, and a value with
		// no spaces needs no quoting (both of ours qualify).
		if dsn != "" {
			dsn += " "
		}
		dsn += name + "=" + value
	}
	return dsn
}

func applyPGURL(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return dsn
	}
	named := map[string]string{}
	for k, vs := range q {
		if len(vs) > 0 {
			named[k] = vs[0]
		}
	}
	if opts, ok := named["options"]; ok {
		for k, v := range pgOptions(opts) {
			named[k] = v
		}
	}
	// Appended textually, before any fragment, so everything the operator
	// wrote stays byte for byte.
	head, frag := dsn, ""
	if i := strings.IndexByte(dsn, '#'); i >= 0 {
		head, frag = dsn[:i], dsn[i:]
	}
	hasQuery := strings.IndexByte(head, '?') >= 0
	for _, p := range PGDSNParams() {
		name, value, _ := strings.Cut(p, "=")
		if have, ok := named[name]; ok {
			if have != value {
				reportKept("postgres", name, value)
			}
			continue
		}
		head = appendQuery(head, hasQuery, p)
		hasQuery = true
	}
	return head + frag
}

// pgKeywords parses a libpq keyword/value connection string: `key = value`
// pairs separated by whitespace, a value either bare or single-quoted, with
// backslash escaping the next character in both. Settings passed through
// `options` are added under their own names. ok is false for a string that
// does not parse.
func pgKeywords(dsn string) (map[string]string, bool) {
	out := map[string]string{}
	i, n := 0, len(dsn)
	skip := func() {
		for i < n && isSpace(dsn[i]) {
			i++
		}
	}
	for {
		skip()
		if i >= n {
			break
		}
		start := i
		for i < n && dsn[i] != '=' && !isSpace(dsn[i]) {
			i++
		}
		key := dsn[start:i]
		skip()
		if key == "" || i >= n || dsn[i] != '=' {
			return nil, false
		}
		i++
		skip()
		var val strings.Builder
		if i < n && dsn[i] == '\'' {
			i++
			closed := false
			for i < n {
				c := dsn[i]
				i++
				if c == '\\' && i < n {
					val.WriteByte(dsn[i])
					i++
					continue
				}
				if c == '\'' {
					closed = true
					break
				}
				val.WriteByte(c)
			}
			if !closed {
				return nil, false
			}
		} else {
			for i < n && !isSpace(dsn[i]) {
				c := dsn[i]
				i++
				if c == '\\' && i < n {
					val.WriteByte(dsn[i])
					i++
					continue
				}
				val.WriteByte(c)
			}
		}
		out[key] = val.String()
	}
	if opts, ok := out["options"]; ok {
		for k, v := range pgOptions(opts) {
			out[k] = v
		}
	}
	return out, true
}

// pgOptions returns the run-time parameters a libpq `options` value sets:
// `-c name=value`, `-cname=value` and `--name=value`, whitespace-separated
// with a backslash escaping a space. PostgreSQL treats `-` and `_` in a
// parameter name alike, so names are normalised to underscores.
func pgOptions(opts string) map[string]string {
	var tokens []string
	var cur strings.Builder
	for i := 0; i < len(opts); i++ {
		c := opts[i]
		switch {
		case c == '\\' && i+1 < len(opts):
			i++
			cur.WriteByte(opts[i])
		case isSpace(c):
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	out := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		var setting string
		switch {
		case t == "-c" && i+1 < len(tokens):
			i++
			setting = tokens[i]
		case strings.HasPrefix(t, "--"):
			setting = t[2:]
		case strings.HasPrefix(t, "-c"):
			setting = t[2:]
		default:
			continue
		}
		name, value, ok := strings.Cut(setting, "=")
		if !ok {
			continue
		}
		out[strings.ReplaceAll(strings.ToLower(name), "-", "_")] = value
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// appendQuery adds one `key=value` entry to a DSN's query string.
func appendQuery(dsn string, hasQuery bool, entry string) string {
	if hasQuery {
		return dsn + "&" + entry
	}
	return dsn + "?" + entry
}

// unescapeQuery URL-decodes a query component the way the drivers do,
// falling back to the raw text when it is not valid escaping.
func unescapeQuery(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// reported remembers which kept settings were already logged, so a process
// that opens many connections says it once.
var reported sync.Map

// reportKept logs that the DSN sets a pinned parameter to a value other
// than the pin. The operator's value is kept — the pins supply a missing
// default, they do not overrule a chosen one — but the emitted SQL was
// written for the pinned value, so the difference is worth one line. Only
// the dialect, the parameter and the pinned value are logged; never the DSN,
// which carries credentials.
func reportKept(dialect, name, pinned string) {
	if _, seen := reported.LoadOrStore(dialect+"\x00"+name, true); seen {
		return
	}
	slog.Warn("dbsession: the DSN sets a session parameter the generated SQL is written for; keeping the DSN's value",
		"dialect", dialect, "parameter", name, "expected", pinned)
}
