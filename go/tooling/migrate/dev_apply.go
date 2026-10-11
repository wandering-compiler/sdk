package migrate

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// DevApply applies a dev diff plan to the local stores — the LOCAL
// execution half of the dev DB lifecycle's diff-apply (the planning half
// moved behind the console API in the thin-client refactor; this stays
// local because it touches the developer's stores directly). Each
// migration's transactional body (with post-tx folded in, CONCURRENTLY
// stripped) is executed via the per-connection Applier resolved from
// applierFor. Nothing is persisted to a migration ledger. The Applier is
// Closed after each connection's apply.
//
// applierFor maps a connection name to its Applier (the same factory the
// migrator uses); an empty connection name is the default bucket, which
// the caller's factory resolves to the project's main connection.
func DevApply(ctx context.Context, plan *applyplanpb.DevApplyPlan, applierFor ApplierFor) error {
	for _, m := range plan.GetMigrations() {
		conn := m.GetConnection()
		applier, err := applierFor(conn)
		if err != nil {
			return fmt.Errorf("devapply: applier for connection %q: %w", conn, err)
		}
		err = applier.Apply(ctx, &applyfetchpb.Migration{UpSql: devApplySQL(m)})
		closeErr := applier.Close()
		if err != nil {
			return fmt.Errorf("devapply: apply to connection %q: %w", conn, err)
		}
		if closeErr != nil {
			return fmt.Errorf("devapply: close connection %q: %w", conn, closeErr)
		}
	}
	return nil
}

// concurrentlyRe strips the CONCURRENTLY keyword (and its surrounding
// whitespace) so a planner-emitted online index build runs inside the
// dev transaction. Case-insensitive; matches the keyword as a whole
// word so it never mangles an identifier that merely contains it.
//
// The word boundary is not enough on its own: it says nothing about WHERE
// in the body the match sits. Applied blind, the
// strip rewrote string literals and comments too — a post-tx body holding
// `VALUES ('built CONCURRENTLY')` came out as `VALUES ('built')`. post_tx is
// where authored raw bodies live, and "the author owns what they persist" is
// a standing owner decision, so a tool silently editing their text breaks it
// even in dev. stripConcurrently skips quoted text and comments.
var concurrentlyRe = regexp.MustCompile(`(?i)\s+CONCURRENTLY\b`)

// stripConcurrently removes the CONCURRENTLY keyword from executable SQL
// while leaving string literals, quoted identifiers, dollar-quoted bodies and
// comments untouched.
//
// It is a scanner rather than a cleverer regexp because the thing being
// decided — "is this offset inside a literal" — is not something a regular
// expression can answer about SQL.
func stripConcurrently(sql string) string {
	var out strings.Builder
	out.Grow(len(sql))
	scanSQL(sql, func(start, end int, executable bool) {
		if executable {
			out.WriteString(concurrentlyRe.ReplaceAllString(sql[start:end], ""))
			return
		}
		out.WriteString(sql[start:end])
	})
	return out.String()
}

// scanSQL walks a Postgres script and hands each consecutive span to visit,
// marking whether it is EXECUTABLE text or text a rewrite must leave alone:
// a `--` or (nested) `/* */` comment, a quoted identifier, a string literal
// (with `E'…'` backslash escapes), or a dollar-quoted body (`$$…$$`,
// `$tag$…$tag$`). The spans cover the input exactly, in order.
//
// One traversal for every rewrite in this file. A function body is the
// common case for a dollar-quoted span, and its text is full of the words
// the rewrites look for — a `COMMIT;` inside a procedure, a CONCURRENTLY in
// a string the function builds — so a scanner that did not know the form
// edited the author's function.
func scanSQL(sql string, visit func(start, end int, executable bool)) {
	run := 0 // start of the pending executable span
	flushTo := func(i int) {
		if i > run {
			visit(run, i, true)
		}
	}
	for i := 0; i < len(sql); {
		end := opaqueEnd(sql, i)
		if end < 0 {
			i++
			continue
		}
		flushTo(i)
		visit(i, end, false)
		i, run = end, end
	}
	flushTo(len(sql))
}

// opaqueEnd reports where the opaque span starting at sql[i] ends, or -1
// when sql[i] starts executable text. An unterminated span runs to the end.
func opaqueEnd(sql string, i int) int {
	switch c := sql[i]; {
	case strings.HasPrefix(sql[i:], "--"):
		if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
			return i + nl
		}
		return len(sql)
	case strings.HasPrefix(sql[i:], "/*"):
		// Postgres block comments nest.
		depth := 0
		for j := i; j < len(sql); {
			switch {
			case strings.HasPrefix(sql[j:], "/*"):
				depth++
				j += 2
			case strings.HasPrefix(sql[j:], "*/"):
				depth--
				j += 2
				if depth == 0 {
					return j
				}
			default:
				j++
			}
		}
		return len(sql)
	case c == '"':
		return quotedEnd(sql, i, '"', false)
	case c == '\'':
		// E'…' (escape string): a backslash escapes the next character.
		escapes := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !isIdentByte(sql[i-2]))
		return quotedEnd(sql, i, '\'', escapes)
	case c == '$':
		if i > 0 && isIdentByte(sql[i-1]) {
			return -1 // `$` inside an identifier, as in a$b
		}
		tag := dollarTagAt(sql, i)
		if tag == "" {
			return -1
		}
		if close := strings.Index(sql[i+len(tag):], tag); close >= 0 {
			return i + len(tag) + close + len(tag)
		}
		return len(sql)
	}
	return -1
}

// quotedEnd returns the index just past the closing quote of the span
// opening at sql[i]. A doubled quote is an escaped one and stays inside.
func quotedEnd(sql string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslash && sql[j] == '\\':
			j++
		case sql[j] == q:
			if j+1 < len(sql) && sql[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(sql)
}

// dollarTagAt returns the `$tag$` delimiter opening at sql[i], or "" when
// none does. A tag follows identifier rules and cannot start with a digit,
// so a positional parameter (`$1`) is not one.
func dollarTagAt(sql string, i int) string {
	j := i + 1
	if j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		return ""
	}
	for j < len(sql) && sql[j] != '$' && isIdentByte(sql[j]) {
		j++
	}
	if j < len(sql) && sql[j] == '$' {
		return sql[i : j+1]
	}
	return ""
}

// isIdentByte reports whether b can continue an unquoted identifier. Bytes
// of a multi-byte UTF-8 rune count, as Postgres lets them into identifiers.
func isIdentByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// devApplySQL renders the SQL dev executes for one migration: the
// transactional up_sql, the post-tx ops with CONCURRENTLY stripped (so they
// run in-transaction without the w17_migrations phase machinery the Applier's
// real post-tx path needs), and finally the applied-ledger baseline when the
// artefact carries one. Empty post-tx ⇒ just up_sql.
//
// **One string, therefore one batch, therefore one transaction** — which is
// the whole point of appending the baseline here rather than applying it as a
// second call. A database left with the schema built and the ledger empty is
// not a state anything recovers from: the next run finds the store populated
// and skips, and the deploy gate then refuses every deploy while naming a
// remedy (`migrate apply`) that would run the series' CREATE against tables
// that already exist. Schema and ledger row land together or neither does.
//
// The baseline is rendered SERVER-side and carries no envelope of its own,
// precisely so it can be folded in here; see applied.BaselineOnly.
func devApplySQL(m *applyplanpb.DevMigration) string {
	// Prerequisites FIRST, in the same statement set as the schema that needs
	// them — `CREATE EXTENSION IF NOT EXISTS`, so a database that already has
	// one is untouched.
	//
	// They used to live only in `db/init/<domain>/00_extensions.sql`, which
	// docker mounts into initdb — and initdb runs on a FRESH VOLUME and never
	// again. A project that declared an extension after its local database
	// existed got that file rewritten and nothing applied it; the failure then
	// arrived at runtime, naming a function nobody had created. A dev database
	// that has been worked in for a week is the normal case, not the exception.
	//
	// Migration BODIES still carry none of this: on a real target, creating an
	// extension is a pre-apply step the deploying platform owns and may need
	// superuser for. This path is the dev one, where the binary applying the
	// schema is the thing holding the connection.
	//
	// OUTSIDE the transaction envelope below, deliberately: a prerequisite is
	// a precondition of the schema rather than part of it, `IF NOT EXISTS`
	// makes re-running it free, and some deployments restrict CREATE
	// EXTENSION inside a transaction.
	var prereq []string

	// Namespaces BEFORE extensions and before the body: a qualified
	// `CREATE TABLE "billing"."accounts"` fails on a database that has no
	// `billing` schema, and nothing in the body creates one. Extensions come
	// after because an extension can be installed INTO a schema.
	//
	// These used to arrive only through `db/init/<domain>/00_extensions.sql`,
	// mounted into one compose container's initdb — so a database built any
	// other way got the qualified DDL and no schema to put it in. The file is
	// gone; the prerequisite travels with the plan that needs it.
	for _, ns := range m.GetRequiredSchemas() {
		if ns = strings.TrimSpace(ns); ns != "" {
			prereq = append(prereq, "CREATE SCHEMA IF NOT EXISTS "+pgQuoteIdent(ns)+";")
		}
	}
	for _, ext := range m.GetRequiredExtensions() {
		if ext = strings.TrimSpace(ext); ext != "" {
			prereq = append(prereq, "CREATE EXTENSION IF NOT EXISTS "+pgQuoteIdent(ext)+";")
		}
	}

	up := m.GetUpSql()
	post := m.GetUpSqlPostTx()
	if post != "" {
		// Safe to run inside a transaction here, which is the whole reason
		// this path can be atomic at all: the CONCURRENTLY that would forbid
		// it has just been stripped.
		post = stripConcurrently(post)
	}
	base := m.GetBaselineSql()

	head := strings.Join(prereq, "\n")

	tail := joinNonEmpty("\n", post, base)
	if tail == "" {
		return joinNonEmpty("\n", head, up)
	}
	if up == "" {
		// Baseline-only, or post-tx-only: adopting a database that already
		// holds its schema. Nothing to be inside of.
		return joinNonEmpty("\n", head, tail)
	}

	// EVERYTHING inside the schema's own transaction when there is one.
	//
	// The schema, its post-tx statements and its applied-ledger baseline
	// have to land together: a built schema with an empty ledger is not a
	// state a retry recovers from, because `storeHasSchema` reads any
	// non-empty fingerprint as "already done" and green-skips that database
	// forever. The crash window is permanent and silent.
	//
	// ⚠️ Two earlier versions each got half of this. Appending everything
	// left the baseline after an explicit COMMIT. Splicing ONLY the
	// baseline in left the post-tx statements outside while the ledger row
	// went inside — the same tear pointing the other way, produced by the
	// fix for it.
	//
	// A body with NO envelope keeps the appended shape: a dialect without
	// transactional DDL has nothing to be inside of, and inventing a BEGIN
	// for it would be a worse answer than the window.
	if i := lastTopLevelCommit(up); i >= 0 {
		return joinNonEmpty("\n", head, up[:i]+tail+"\n\n"+up[i:])
	}
	return joinNonEmpty("\n", head, up+"\n"+tail)
}

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// commitRe is a transaction-ending COMMIT statement, in any case and with
// the optional noise word.
var commitRe = regexp.MustCompile(`(?i)\bCOMMIT(?:\s+(?:WORK|TRANSACTION))?\s*;`)

// lastTopLevelCommit returns the index of the final `COMMIT;` that is real
// SQL — not one sitting inside a comment, a quoted string or a dollar-quoted
// body. Returns -1 when the body carries no transaction envelope.
//
// ⚠️ A substring search is not good enough and that is measured, not
// theoretical: a post-tx statement containing `'after COMMIT; rebuild
// stats'` had the baseline spliced into the middle of the author's string
// literal. A procedure body (`$$ … COMMIT; … $$`) is the same mistake
// inside a dollar quote, and a lower-case `commit;` envelope was not seen at
// all, which put the baseline AFTER the transaction it belongs in. The
// traversal is scanSQL, shared with stripConcurrently: two scanners over the
// same grammar drift, and the one that drifts is the one nobody reads.
func lastTopLevelCommit(sql string) int {
	last := -1
	scanSQL(sql, func(start, end int, executable bool) {
		if !executable {
			return
		}
		if locs := commitRe.FindAllStringIndex(sql[start:end], -1); len(locs) > 0 {
			last = start + locs[len(locs)-1][0]
		}
	})
	return last
}
