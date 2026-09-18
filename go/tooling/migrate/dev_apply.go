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
// in the body the match sits (T2-5 pass #12, A12-12). Applied blind, the
// strip rewrote string literals and comments too — a post-tx body holding
// `VALUES ('built CONCURRENTLY')` came out as `VALUES ('built')`. post_tx is
// where authored raw bodies live, and "the author owns what they persist" is
// a standing owner decision, so a tool silently editing their text breaks it
// even in dev. stripConcurrently skips quoted text and comments.
var concurrentlyRe = regexp.MustCompile(`(?i)\s+CONCURRENTLY\b`)

// stripConcurrently removes the CONCURRENTLY keyword from executable SQL
// while leaving string literals, quoted identifiers and comments untouched.
//
// It is a scanner rather than a cleverer regexp because the thing being
// decided — "is this offset inside a literal" — is not something a regular
// expression can answer about SQL.
func stripConcurrently(sql string) string {
	var out strings.Builder
	out.Grow(len(sql))
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				out.WriteString(sql[i:])
				return out.String()
			}
			out.WriteString(sql[i : i+end])
			i += end
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				out.WriteString(sql[i:])
				return out.String()
			}
			out.WriteString(sql[i : i+2+end+2])
			i += 2 + end + 2
		case sql[i] == '\'' || sql[i] == '"':
			q := sql[i]
			j := i + 1
			for j < len(sql) {
				if sql[j] == q {
					// A doubled quote is an escaped one and stays inside.
					if j+1 < len(sql) && sql[j+1] == q {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			out.WriteString(sql[i:j])
			i = j
		default:
			// Executable run: up to the next literal/comment opener.
			j := i
			for j < len(sql) {
				if sql[j] == '\'' || sql[j] == '"' ||
					strings.HasPrefix(sql[j:], "--") || strings.HasPrefix(sql[j:], "/*") {
					break
				}
				j++
			}
			out.WriteString(concurrentlyRe.ReplaceAllString(sql[i:j], ""))
			i = j
		}
	}
	return out.String()
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
			prereq = append(prereq, `CREATE SCHEMA IF NOT EXISTS "`+ns+`";`)
		}
	}
	for _, ext := range m.GetRequiredExtensions() {
		if ext = strings.TrimSpace(ext); ext != "" {
			prereq = append(prereq, `CREATE EXTENSION IF NOT EXISTS "`+ext+`";`)
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
	// left the baseline after an explicit COMMIT (D14-4). Splicing ONLY the
	// baseline in left the post-tx statements outside while the ledger row
	// went inside — the same tear pointing the other way, produced by the
	// fix for it (T3-7 pass #15, C15-9).
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

// lastTopLevelCommit returns the index of the final `COMMIT;` that is real
// SQL — not one sitting inside a comment or a quoted string. Returns -1 when
// the body carries no transaction envelope.
//
// ⚠️ A substring search is not good enough and that is measured, not
// theoretical: a post-tx statement containing `'after COMMIT; rebuild
// stats'` had the baseline spliced into the middle of the author's string
// literal (T3-7 pass #15, C15-9). The correct scanner already existed in
// this file — `stripConcurrently` walks comments and quoted strings — and
// simply was not used. Reusing its traversal rather than writing a second
// one is the point: two scanners over the same grammar drift, and the one
// that drifts is the one nobody reads.
func lastTopLevelCommit(sql string) int {
	const needle = "COMMIT;"
	last := -1
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return last
			}
			i += end
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return last
			}
			i += 2 + end + 2
		case sql[i] == '\'' || sql[i] == '"':
			q := sql[i]
			j := i + 1
			for j < len(sql) {
				if sql[j] == q {
					if j+1 < len(sql) && sql[j+1] == q {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			i = j
		default:
			if strings.HasPrefix(sql[i:], needle) {
				last = i
				i += len(needle)
				continue
			}
			i++
		}
	}
	return last
}
