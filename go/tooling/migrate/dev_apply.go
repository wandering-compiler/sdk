package migrate

import (
	"context"
	"fmt"
	"regexp"

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
var concurrentlyRe = regexp.MustCompile(`(?i)\s+CONCURRENTLY\b`)

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
	return sql + "\n" + concurrentlyRe.ReplaceAllString(post, "")
}
