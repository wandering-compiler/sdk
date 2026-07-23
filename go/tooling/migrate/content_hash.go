package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ContentHash is the canonical keyless integrity hash a migration's
// content_sha256 field carries. It is the SINGLE source of truth used by BOTH
// the console (which computes it at store time) and the apply tool (which
// re-computes + compares it at load/apply time) — see the console registry's
// storeMigrations / PushRawMigration and the orchestrator's load + WriteMigration
// paths.
//
// It covers ALL FOUR migration segments, not just up_sql (writer-F2/sign-F5):
// the down_sql / down_pre_tx / up_post_tx bodies execute on the client too, and
// before this they rode entirely outside the keyless client-side integrity check
// (they are ed25519-verified server-side at fetch, but nothing re-anchored them
// after the artifact landed on disk). The encoding is injective — a versioned
// tag plus decimal length-prefix per segment — so no two distinct segment tuples
// can collide by shifting bytes across the (empty-delimited) boundaries.
func ContentHash(up, upPostTx, downPreTx, downSql string) string {
	var b strings.Builder
	b.WriteString("w17.content.v1\n")
	writeSeg(&b, "up", up)
	writeSeg(&b, "post", upPostTx)
	writeSeg(&b, "downpre", downPreTx)
	writeSeg(&b, "down", downSql)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func writeSeg(b *strings.Builder, tag, body string) {
	b.WriteString(tag)
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(len(body)))
	b.WriteByte('\n')
	b.WriteString(body)
	b.WriteByte('\n')
}
