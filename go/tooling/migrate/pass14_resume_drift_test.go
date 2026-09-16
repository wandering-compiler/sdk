package migrate

import (
	"context"
	"io"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// resumeApplier is a target whose in-tx half is already committed: the
// phase is Pending, and its schema therefore reads as the POST-in-tx state
// rather than the pre-state the migration was planned against.
type resumeApplier struct {
	fingerprint string
	postTxRuns  int
}

func (a *resumeApplier) Apply(context.Context, *applyfetchpb.Migration) error    { return nil }
func (a *resumeApplier) AppliedHead(context.Context) (string, error)             { return "", nil }
func (a *resumeApplier) Close() error                                            { return nil }
func (a *resumeApplier) Rollback(context.Context, *applyfetchpb.Migration) error { return nil }
func (a *resumeApplier) Fingerprint(context.Context) (string, error)             { return a.fingerprint, nil }

func (a *resumeApplier) MigrationPhase(context.Context, string) (Phase, error) {
	return PhasePending, nil
}

func (a *resumeApplier) ApplyPostTx(context.Context, *applyfetchpb.Migration) error {
	a.postTxRuns++
	return nil
}

// TestPreFingerprint_DoesNotWedgeAResume — T3-7 pass #14, `D14-3`.
//
// The drift gate landed days before this pass and closed a real hole: it
// refuses to apply a migration onto a database that is not in the state the
// migration was planned against. `expected_pre_fingerprint` had been stamped
// and compared nowhere for three passes, and automating apply is what made
// that urgent.
//
// Its closure introduced this. The check runs BEFORE `applyOrResume`, so it
// also meets a migration whose in-tx half is already committed — a process
// killed mid-skirt, which is precisely the case the resume machinery exists
// for. That database is at the POST-in-tx state by construction, so the
// pre-state comparison fails and the refusal says "hand-applied DDL, or a
// restore from another environment". Every redeploy repeats it: a permanent
// wedge, described as somebody's manual tampering.
//
// A pre-state check is a question about a migration that has NOT STARTED.
// For one that has, the pre-state was true when it began and cannot be true
// now — asking is not a stricter check, it is the wrong question.
func TestPreFingerprint_DoesNotWedgeAResume(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		// The header the console stamps: the state expected BEFORE this
		// migration runs.
		UpSql: "-- w17:expected_pre_fingerprint: pre-state-hash\nSELECT 1;",
	}
	a := &resumeApplier{fingerprint: "post-in-tx-hash"}

	var out strings.Builder
	if err := checkPreFingerprint(context.Background(), a, m, io.Discard); err != nil {
		t.Fatalf("the drift gate refused a migration that is mid-flight:\n%v\n\n"+
			"Its in-tx half is committed, so the database is at the post-in-tx state by "+
			"construction — the pre-state can never match again. The refusal blames a "+
			"hand-applied DDL and every redeploy repeats it (T3-7 pass #14, D14-3).", err)
	}
	if err := applyOrResume(context.Background(), a, m, &out); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if a.postTxRuns != 1 {
		t.Errorf("post-tx phase ran %d time(s), want 1 — the resume did not happen", a.postTxRuns)
	}
}

// The gate must still refuse a migration that has NOT started against a
// drifted database — otherwise this fix would trade one hole for the hole
// the gate was built to close.
func TestPreFingerprint_StillRefusesRealDrift(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql:      "-- w17:expected_pre_fingerprint: pre-state-hash\nSELECT 1;",
	}
	a := &freshDriftedApplier{fingerprint: "somebody-elses-schema"}
	if err := checkPreFingerprint(context.Background(), a, m, io.Discard); err == nil {
		t.Fatal("a migration that has not started was applied onto a drifted database")
	}
}

type freshDriftedApplier struct{ fingerprint string }

func (a *freshDriftedApplier) Apply(context.Context, *applyfetchpb.Migration) error    { return nil }
func (a *freshDriftedApplier) AppliedHead(context.Context) (string, error)             { return "", nil }
func (a *freshDriftedApplier) Close() error                                            { return nil }
func (a *freshDriftedApplier) Rollback(context.Context, *applyfetchpb.Migration) error { return nil }
func (a *freshDriftedApplier) Fingerprint(context.Context) (string, error)             { return a.fingerprint, nil }

func (a *freshDriftedApplier) MigrationPhase(context.Context, string) (Phase, error) {
	return PhaseFresh, nil
}
func (a *freshDriftedApplier) ApplyPostTx(context.Context, *applyfetchpb.Migration) error { return nil }
