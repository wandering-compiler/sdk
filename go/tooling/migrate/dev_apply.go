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
// transactional up_sql followed by the post-tx ops with CONCURRENTLY
// stripped (so they run in-transaction without the wc_migrations phase
// machinery the Applier's real post-tx path needs). Empty post-tx ⇒
// just up_sql.
func devApplySQL(m *applyplanpb.DevMigration) string {
	sql := m.GetUpSql()
	post := m.GetUpSqlPostTx()
	if post == "" {
		return sql
	}
	return sql + "\n" + stripConcurrently(post)
}
