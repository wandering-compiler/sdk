package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
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
//
// # The chain (T2-5 B11-1)
//
// `prev` is the PREDECESSOR's content hash, and covering it is what makes the
// signed lock reach past the migration it names. The lock pins one hash, for
// the TARGET migration; every migration applied on the way there was anchored
// only by its own self-hash, which an attacker holding the artifact directory
// recomputes for free. Measured live before this: a tampered intermediate
// executed verbatim, exit 0.
//
// With the predecessor's hash INSIDE the hashed input, tampering with any
// migration changes its hash, which changes its successor's hashed input,
// and so on up to the target — whose hash the signed lock pins. One pinned
// value now covers the whole chain. `prev` must stay an INPUT to the hash and
// never merely a field beside it: a value the artifact carries next to its own
// hash is a value the attacker rewrites along with everything else.
//
// # Encoding versions
//
// An empty `prev` emits the v1 encoding, byte-identical to what this function
// produced before the chain existed — so every migration stored by an older
// console keeps its hash and keeps verifying. A non-empty `prev` emits v2,
// which appends the predecessor segment under a distinct version tag. The two
// tags make the encodings disjoint: a chained migration cannot be re-hashed as
// an unchained one without changing its hash, so the chain cannot be stripped.
//
// A chain root — the first migration on a connection — legitimately has no
// predecessor, and hashes as v1 only when nothing ELSE is set either: v1
// requires empty `prev` AND empty `supersedes` AND empty `adopt_sql`.
//
// A squash baseline is therefore NOT v1, despite starting the live history
// over: it carries `supersedes` (and usually `adopt_sql`), so it hashes as v2
// and its digest changed under F1 — which is exactly what the §T2-5-pass-#12
// paragraph below says, in the opposite words. This paragraph claimed the
// baseline hashed as v1 until T2-5 pass #14 (D14-8), and the two of them
// answered the one operational question this section exists for — "which
// stored digests changed?" — differently depending on which the reader
// reached first.
// # What else decides what executes (T2-5 pass #12)
//
// The four bodies are not the whole story, and the first version of the chain
// treated them as if they were. `supersedes` and `adopt_sql` decide whether a
// migration's real DDL runs AT ALL and supply the SQL that runs instead: the
// apply path flips a migration to "adopt" when its `supersedes` names the
// database's applied head, then executes `adopt_sql` and skips `up_sql`.
// Measured before this covered them — tampering only those two fields on an
// intermediate artefact, leaving `content_sha256` untouched, ran attacker SQL
// and suppressed the real migration, exit 0.
//
// They ride outside the console's ed25519 signature too (that binding covers
// id / direction / project / connection / up / post), so before this there was
// no mechanism anywhere on the path that covered them.
//
// Both are emitted only when set, so an ordinary migration — which has
// neither — keeps the hash it already had. Only a squash baseline's digest
// changes, and a baseline whose stored hash predates this needs a re-push.
//
// # What is deliberately NOT hashed, and why
//
// `id` and `connection` are NOT inputs, and that is a decision rather than an
// oversight — see TestContentHash_EveryMigrationFieldIsHashedOrExcused, which
// forces every field of the artefact to be one or the other with a reason:
//
//   - `connection` — the directory the artefact lives in is the authority
//     (`loadConnectionMigrations` backfills the field from it), so hashing it
//     would bind a value nothing trusts.
//   - `id` — bound by ORDER instead: the console assigns ids monotonically per
//     connection, so `chainFromTarget` requires ids to increase along the
//     chain. That is what catches a relabel; hashing the id would change every
//     stored digest to close the same hole.
func ContentHash(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, manifestJSON string) string {
	// The manifest is bound by its PROJECTION, not by its bytes.
	//
	// `manifest_json` is protojson over a message the compiler keeps growing,
	// so hashing the blob would move the digest of a migration whose SQL never
	// changed the first time the console added a field — every pin in every
	// lock would break on a release that changed nothing about what executes.
	//
	// What the client acts on is the required-extension set, and that is what
	// gets hashed: sorted and deduplicated, because the set is the semantics.
	// Reordering it is not an edit anybody can act on, unlike `supersedes`
	// below, where order is deliberately part of the digest.
	//
	// Not excused as metadata, though the rest of the manifest is: an empty
	// list means "no prerequisites, proceed", so stripping the list from a
	// fetched artifact LICENSES a run that would have been refused. That is
	// the opposite direction from adopt_preflight_sql, which fails closed and
	// is excused for exactly that reason.
	exts := canonicalExtensions(requiredExtensionsFromManifest(manifestJSON))

	var b strings.Builder
	if prev == "" && len(supersedes) == 0 && adoptSQL == "" && len(exts) == 0 {
		b.WriteString("w17.content.v1\n")
	} else {
		b.WriteString("w17.content.v2\n")
	}
	writeSeg(&b, "up", up)
	writeSeg(&b, "post", upPostTx)
	writeSeg(&b, "downpre", downPreTx)
	writeSeg(&b, "down", downSql)
	if prev != "" {
		writeSeg(&b, "prev", prev)
	}
	// The count is written before the elements so a list cannot be confused
	// with a single element containing the same bytes, and the elements keep
	// their given order — `supersedes` is a set semantically, but reordering it
	// is still an edit the digest should notice.
	if len(supersedes) > 0 {
		writeSeg(&b, "supersedes.n", strconv.Itoa(len(supersedes)))
		for _, s := range supersedes {
			writeSeg(&b, "supersedes", s)
		}
	}
	if adoptSQL != "" {
		writeSeg(&b, "adopt", adoptSQL)
	}
	// Written only when non-empty, so every digest minted before the manifest
	// travelled keeps its value. A migration declaring no extensions hashes
	// exactly as it did before this segment existed.
	if len(exts) > 0 {
		writeSeg(&b, "reqext.n", strconv.Itoa(len(exts)))
		for _, e := range exts {
			writeSeg(&b, "reqext", e)
		}
	}
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

// canonicalExtensions sorts and deduplicates, so the digest binds the SET the
// preflight enforces rather than the order the manifest happened to serialise.
func canonicalExtensions(exts []string) []string {
	if len(exts) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(exts))
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
