package migrate

import (
	"context"
	"fmt"
	"io"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The drift gate: refuse a migration whose recorded starting state is not the
// state the target database is actually in.
//
// `expected_pre_fingerprint` has been stamped onto every migration since Phase
// D, parsed back out by the console, carried across a rescope — and compared
// NOWHERE. `FingerprintCapable` was declared here, implemented by all three
// relational appliers, and never called by anything. The header and the
// capability were two halves of a check that was never joined up.
//
// It mattered less while a person typed `migrate apply` and watched. It stops
// being tolerable the moment a deploy applies migrations unattended, because
// this is precisely the check whose job is to say "do not run this, the
// database is not where you think it is" — the protection an operator would
// most reasonably assume is the reason automating the apply is safe.
//
// The comparison is possible at all because both sides already compute the
// fingerprint with the SAME code: the console's shadow-DB provider calls
// `fingerprint.ExtractPostgres` + `FingerprintHex` (schemas/client.go), and so
// does the applier that runs against the real database
// (migrate/internal/postgres). Same package, same functions — the two values
// are comparable by construction rather than by convention.

// fakeFingerprintPrefix marks the placeholder the console stamps when it has
// no shadow-DB provider for a connection's dialect. It is not a fingerprint of
// anything and must never be compared against one.
const fakeFingerprintPrefix = "FAKE_"

// preFingerprintMarker is the header key, without the comment prefix.
const preFingerprintMarker = "wc:expected_pre_fingerprint: "

// ExpectedPreFingerprint reads the `expected_pre_fingerprint` header a
// migration carries, reporting whether one was present.
//
// The header is the FIRST line of the forward body — of `up_sql`, or of
// `up_post_tx` when the in-transaction half is empty. That is the shape the
// console writes and the shape it parses back.
//
// Both comment prefixes are accepted, and that is not defensive breadth: SQL
// bodies are commented with `--`, while a data migration's manifest uses `#`,
// and a data migration can target a relational connection. Keying on the
// marker rather than on the prefix means the gate does not have to know which
// kind of body it is holding.
func ExpectedPreFingerprint(m *applyfetchpb.Migration) (string, bool) {
	for _, body := range []string{m.GetUpSql(), m.GetUpPostTx()} {
		if v, ok := firstLineHeader(body); ok {
			return v, true
		}
	}
	return "", false
}

// firstLineHeader pulls the marker's value off the first line of body.
func firstLineHeader(body string) (string, bool) {
	if body == "" {
		return "", false
	}
	line := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line = body[:i]
	}
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"--", "#"} {
		token := prefix + " " + preFingerprintMarker
		if strings.HasPrefix(line, token) {
			return strings.TrimSpace(strings.TrimPrefix(line, token)), true
		}
	}
	return "", false
}

// checkPreFingerprint refuses to apply m when the target database is not in
// the state m was planned against.
//
// It SKIPS — rather than refuses — in three cases, each because the question
// genuinely does not apply, not because the answer is inconvenient:
//
//   - the applier is not FingerprintCapable. Schemaless targets (Redis, NATS,
//     S3) have no schema state for "is the target where I think it is" to be a
//     question about.
//   - the header is the `FAKE_` placeholder. The console stamps it for a
//     dialect its shadow-DB matrix does not cover; it is a well-formed header
//     with no fingerprint behind it, and comparing against it would refuse
//     every migration on those connections.
//   - the applier reports an empty fingerprint, which its contract reserves
//     for "no applicable schema" (a fresh database with no tables).
//
// A MISSING header also skips, and that one deserves saying out loud: header
// presence is enforced by the console when it serves the migration
// (decorate.Parse refuses a body without one), so a body that reaches here
// without a header did not come through that path — a dev-mode apply, or a
// hand-written fixture. Refusing here would turn those into failures without
// making any fetched migration safer, since a fetched one always has the
// header. If that ever stops being true, this is the line to revisit.
func checkPreFingerprint(ctx context.Context, applier Applier, m *applyfetchpb.Migration, out io.Writer) error {
	want, ok := ExpectedPreFingerprint(m)
	if !ok || strings.HasPrefix(want, fakeFingerprintPrefix) {
		return nil
	}
	fp, ok := applier.(FingerprintCapable)
	if !ok {
		return nil
	}
	got, err := fp.Fingerprint(ctx)
	if err != nil {
		return fmt.Errorf("drift check for %s: read the target's schema state: %w", m.GetId(), err)
	}
	if got == "" || got == want {
		return nil
	}
	fmt.Fprintf(out, "apply:   REFUSED %s — target schema is not the state this migration was planned against\n", m.GetId())
	return fmt.Errorf(
		"drift check for %s: the target database is not in the state this migration was planned against\n"+
			"  expected pre-state: %s\n"+
			"  target is at:       %s\n"+
			"  why: the migration records the schema it was generated to run after. A different value means the\n"+
			"       database changed outside this migration history — a hand-applied DDL, a restore from another\n"+
			"       environment, or a different project's database behind this connection.\n"+
			"  fix: reconcile the database with its history before applying. `migrate status` shows what this\n"+
			"       connection believes is applied; `migrate adopt` is how a database that already holds the\n"+
			"       schema is brought under management.",
		m.GetId(), want, got,
	)
}
