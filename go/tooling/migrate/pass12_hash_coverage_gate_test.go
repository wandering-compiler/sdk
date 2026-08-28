package migrate_test

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// T2-5 pass #12 — the CLASS behind three of this pass's findings.
//
// `supersedes`, `adopt_sql` and `id` were each found separately, by different
// lanes, and each is the same shape: `ContentHash` is a hand-written segment
// list over a proto that keeps growing, and the fields nobody remembered to
// add are the ones that decide what executes. That shape — "the enumeration
// behind the proto" — is prior #1 in this dimension's brief, and the B11-1 fix
// reproduced it while closing an instance of it.
//
// So this gate, not three more segments. Every field of the artefact must be
// either an INPUT to the digest or explicitly excused WITH a reason, and a
// field added tomorrow fails here until somebody decides which it is. Same
// technique as the two classes closed on 2026-08-15
// (lock.TestCanonicalize_EveryRepeatedFieldIsClassified,
// compat.TestColumnAxes_EveryFieldIsSeenOrExcused): walk the DESCRIPTOR, never
// a list a human has to keep in sync.
func TestContentHash_EveryMigrationFieldIsHashedOrExcused(t *testing.T) {
	excused := map[string]string{
		"content_sha256": "the digest itself — it cannot be an input to its own computation",
		"connection": "the DIRECTORY the artifact lives in is the authority — routing reads `ct.Connection`, " +
			"never this field, so hashing it would bind a value nothing acts on. " +
			"loadConnectionMigrations backfills it only when EMPTY (a self-heal for older fetches), " +
			"so a wrong non-empty value survives into logging and adopt bookkeeping and is not " +
			"corrected — which is why this is excused as unauthoritative rather than as derived " +
			"(T2-5 pass #14, C14-13: the excuse used to claim the loader rewrites it unconditionally)",
		"adopt_preflight_sql": "fail-CLOSED, so stripping it cannot widen what an attacker can do. " +
			"It is the check `migrate adopt` must pass before recording a migration it will not run, " +
			"and an EMPTY value means REFUSE rather than \"nothing to check\" (RunAdopt), so tampering " +
			"with it can only prevent an adoption, never license one. Hashing it would change every " +
			"stored digest and every lock pin in existence to close a hole that is already shut in the " +
			"one direction that matters. The statements it carries are also never applied as a " +
			"migration: apply runs up_sql, and only the explicit adopt path runs this",
		"id": "bound by ORDER instead: the console assigns ids monotonically per connection, " +
			"and chainFromTarget requires them to increase along the chain. Hashing it would " +
			"change every stored digest to close a hole the order check closes for free — see " +
			"TestPlan_RelabelledIdIsRefused",
	}

	fields := (&applyfetchpb.Migration{}).ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		name := string(fd.Name())
		if reason, ok := excused[name]; ok {
			if len(reason) < 40 {
				t.Errorf("field %q is excused, but the reason is too thin to review: %q", name, reason)
			}
			continue
		}
		if !fieldMovesTheDigest(t, fd) {
			t.Errorf("field %q travels with the artifact but does not change ContentHash.\n"+
				"Every field is either an input to the digest or excused WITH a reason. If it cannot "+
				"affect what executes on the target database, add it to `excused` and say why; if it "+
				"can, hash it. Three findings in pass #12 were fields that quietly took the first "+
				"option without anybody choosing it.", name)
		}
	}
}

// fieldMovesTheDigest reports whether CHANGING fd's value changes the digest.
//
// Both probes set the field to a NON-EMPTY value — two different ones — rather
// than comparing empty against set. That distinction is the whole strength of
// this gate, and getting it wrong made the first version of it vacuous: the
// encoding picks its version tag on whether the optional segments are present
// at all, so an empty→set probe moves the digest through the TAG even when the
// field's value is not covered. Measured while break-proofing this pass —
// deleting the `adopt_sql` segment from ContentHash left the gate green.
//
// Driven off the descriptor rather than a switch on field names, so a new
// field is covered the day it lands rather than the day somebody remembers
// this test exists.
func fieldMovesTheDigest(t *testing.T, fd protoreflect.FieldDescriptor) bool {
	t.Helper()
	probe := func(v string) *applyfetchpb.Migration {
		m := &applyfetchpb.Migration{Id: "ts-1", Connection: "main", UpSql: "SELECT 1;"}
		r := m.ProtoReflect()
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.StringKind:
			r.Mutable(fd).List().Append(protoreflect.ValueOfString(v))
		case !fd.IsList() && fd.Kind() == protoreflect.StringKind:
			r.Set(fd, protoreflect.ValueOfString(v))
		default:
			// Refusing to guess is the point: a field shape this gate cannot
			// probe is a field whose coverage nobody has established.
			t.Fatalf("field %q has shape (list=%v kind=%s) that this gate cannot probe — teach it, do not skip it",
				fd.Name(), fd.IsList(), fd.Kind())
		}
		return m
	}
	return digestOf(probe("zzz-probe-a")) != digestOf(probe("zzz-probe-b"))
}

func digestOf(m *applyfetchpb.Migration) string {
	return migrate.ContentHash(
		m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(),
		m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql())
}
