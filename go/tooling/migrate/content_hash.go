package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// ContentHash is the canonical keyless integrity hash a migration's
// content_sha256 field carries. It is the SINGLE source of truth used by BOTH
// the console (which computes it at store time) and the apply tool (which
// re-computes + compares it at load/apply time) — see the console registry's
// storeMigrations / PushRawMigration and the orchestrator's load + WriteMigration
// paths.
//
// It covers ALL FOUR migration segments, not just up_sql:
// the down_sql / down_pre_tx / up_post_tx bodies execute on the client too, and
// before this they rode entirely outside the keyless client-side integrity check
// (they are ed25519-verified server-side at fetch, but nothing re-anchored them
// after the artifact landed on disk). The encoding is injective — a versioned
// tag plus decimal length-prefix per segment — so no two distinct segment tuples
// can collide by shifting bytes across the (empty-delimited) boundaries.
//
// # The chain
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
// and its digest changed when `supersedes` and `adopt_sql` became hash
// inputs — which is exactly what the paragraph below says. An earlier
// version of this paragraph claimed the baseline hashed as v1, and the two
// of them then answered the one operational question this section exists
// for — "which stored digests changed?" — differently depending on which
// the reader reached first.
//
// # What else decides what executes
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
//
// # The adopt preflight
//
// `adopt_preflight_sql` is not an input to ContentHash; it is to
// [ContentHashWithPreflight]. It was once excused on the grounds that an empty
// preflight refuses, so tampering could only block an adoption. That reasoning
// covered stripping it and missed replacing it: `--fake` EXECUTES the
// preflight, so a replaced body runs as written, and a trivially true one
// licenses an adoption the real check would have refused.
//
// It could not simply be added here: every digest already minted for a
// migration that carries a preflight would change, and with it every lock pin
// on such a migration. So the binding is a separate formula, and the one
// place that runs the preflight — `--fake` — refuses an artifact whose digest
// does not bind it (see preflightBound). Everything else keeps verifying
// against the digests it already has.
func ContentHash(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, manifestJSON string) string {
	return ContentHashWithPreflight(up, upPostTx, downPreTx, downSql, prev, supersedes, adoptSQL, "", manifestJSON)
}

// ContentHashWithPreflight is [ContentHash] with `adopt_preflight_sql` bound as
// well. With an empty preflight it is byte-identical to ContentHash, so only a
// migration that carries a preflight gets a different digest — and that is the
// digest an artifact needs for `--fake` to run its preflight.
func ContentHashWithPreflight(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, adoptPreflightSQL, manifestJSON string) string {
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
	// the same direction as a replaced adopt_preflight_sql, which is why that
	// one is bound too (by ContentHashWithPreflight).
	return ContentHashWithChecks(up, upPostTx, downPreTx, downSql, prev, supersedes, adoptSQL, adoptPreflightSQL, manifestJSON, nil)
}

// ContentHashWithChecks is [ContentHashWithPreflight] with the migration's
// pre-apply checks bound too, so a check is exactly as trusted as the bodies:
// `apply --checks` EXECUTES them, and an unbound check is SQL anybody holding
// the artifact could have replaced. With no checks it is byte-identical to
// ContentHashWithPreflight, so every digest minted for a migration without
// checks keeps its value.
//
// The checks are hashed as a SET in a canonical order (direction, then name):
// the console serves them sorted, but a reordering is not an edit anything
// acts on. Each contributes its name, direction, SQL and rationale — the
// rationale is printed in the refusal, and a replaced one could misdirect the
// operator.
func ContentHashWithChecks(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, adoptPreflightSQL, manifestJSON string, checks []*applyfetchpb.Check) string {
	exts := canonicalExtensions(requiredExtensionsFromManifest(manifestJSON))
	return contentHash(up, upPostTx, downPreTx, downSql, prev, supersedes, adoptSQL, adoptPreflightSQL, exts, checks)
}

// ChecksBound reports whether m's content_sha256 covers its checks —
// trivially so when it carries none. The loader refuses an artifact whose
// checks are not bound, and `apply --checks` asks again before it runs one.
func ChecksBound(m *applyfetchpb.Migration) bool {
	if len(m.GetChecks()) == 0 {
		return true
	}
	want := m.GetContentSha256()
	return want != "" && ContentHashWithChecks(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), m.GetAdoptPreflightSql(), m.GetManifestJson(), m.GetChecks()) == want
}

// ContentHashMatches reports whether `want` is a digest this content can
// legitimately carry — under the CURRENT formula, or under the one that
// applied before the required-extension segment existed.
//
// It exists because adding that segment was not the backward-compatible
// change it was reasoned to be. The guard was "written only when non-empty,
// so every digest minted before the manifest travelled keeps its value" —
// true for a migration that declares NO extensions, and false for exactly
// the ones that do. Those were stored, months ago, hashed without the
// segment; a client carrying the new formula recomputes them differently and
// `WriteMigration` refuses the fetch with a message about hand-editing. The
// evidence the change relied on — "verified against the production console:
// its one stored migration carries no manifest" — was an existence check over
// ONE deployment, not over the shape (measured).
//
// Accepting the legacy digest is not a weakening. The segment's purpose is
// that STRIPPING the extension list from an artifact must not license a run
// the preflight would refuse — and a legacy digest is only accepted when the
// content hashes to it under the legacy formula, which pins every executable
// segment exactly as before. What it cannot detect is a strip on an artifact
// minted before the segment existed, which is the state those artifacts were
// already in; it does not make anything worse, and every digest minted from
// here on is bound.
func ContentHashMatches(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, manifestJSON, want string) bool {
	if want == "" {
		return false
	}
	exts := canonicalExtensions(requiredExtensionsFromManifest(manifestJSON))
	if contentHash(up, upPostTx, downPreTx, downSql, prev, supersedes, adoptSQL, "", exts, nil) == want {
		return true
	}
	if len(exts) == 0 {
		// No segment either way — one formula, and it did not match.
		return false
	}
	// The pre-segment formula: identical but for the extension set.
	return contentHash(up, upPostTx, downPreTx, downSql, prev, supersedes, adoptSQL, "", nil, nil) == want
}

// migrationHashMatches is the artifact check the loader and WriteMigration
// apply: the digest is either one that binds the adopt preflight, or one of
// the formulas [ContentHashMatches] accepts, which do not.
func migrationHashMatches(m *applyfetchpb.Migration) bool {
	// An artifact that carries checks verifies only under the formula that
	// binds them: checks travel only on migrations whose digest binds them, so
	// one that does not bind them was edited (or its checks were added).
	if len(m.GetChecks()) > 0 {
		return ChecksBound(m)
	}
	if m.GetAdoptPreflightSql() != "" && preflightBound(m) {
		return true
	}
	return ContentHashMatches(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), m.GetManifestJson(), m.GetContentSha256())
}

// preflightBound reports whether m's content_sha256 covers its
// `adopt_preflight_sql` — trivially so when it carries none. A digest minted
// by [ContentHash] for a migration that does carry one verifies everything
// else but leaves the preflight free to be replaced, so it must not be run on
// the strength of that digest.
func preflightBound(m *applyfetchpb.Migration) bool {
	if m.GetAdoptPreflightSql() == "" {
		return true
	}
	want := m.GetContentSha256()
	return want != "" && ContentHashWithChecks(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql(), m.GetAdoptPreflightSql(), m.GetManifestJson(), m.GetChecks()) == want
}

func contentHash(up, upPostTx, downPreTx, downSql, prev string, supersedes []string, adoptSQL, adoptPreflightSQL string, exts []string, checks []*applyfetchpb.Check) string {

	var b strings.Builder
	if prev == "" && len(supersedes) == 0 && adoptSQL == "" && adoptPreflightSQL == "" && len(exts) == 0 && len(checks) == 0 {
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
	// Written only when non-empty, so a migration without a preflight keeps
	// the digest it already had.
	if adoptPreflightSQL != "" {
		writeSeg(&b, "adoptpre", adoptPreflightSQL)
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
	// Written only when non-empty, so a migration without checks keeps the
	// digest it had before checks were bound.
	if len(checks) > 0 {
		sorted := append([]*applyfetchpb.Check(nil), checks...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].GetDown() != sorted[j].GetDown() {
				return !sorted[i].GetDown()
			}
			return sorted[i].GetName() < sorted[j].GetName()
		})
		writeSeg(&b, "checks.n", strconv.Itoa(len(sorted)))
		for _, c := range sorted {
			dir := "up"
			if c.GetDown() {
				dir = "down"
			}
			writeSeg(&b, "check.name", c.GetName())
			writeSeg(&b, "check.dir", dir)
			writeSeg(&b, "check.sql", c.GetSql())
			writeSeg(&b, "check.why", c.GetRationale())
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
