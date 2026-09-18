package migrate

import (
	"context"
	"os"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// TestApply_SkipsWhatAnotherRunAppliedWhileWeQueued — T3-7 pass #14,
// `D14-2`, and pass #15's `C15-10`.
//
// ⚠️ THE FIRST VERSION OF THIS TEST CERTIFIED NOTHING. It computed
// `tc.head != "" && tc.id <= tc.head` — its own expression, over its own
// table — and imported no production code at all. Measured in pass #15:
// deleting the ENTIRE apply-path post-lock re-read and skip left the whole
// migrate suite green, and so did disabling the rollback refusal. Only
// removing the literal string `AppliedHead` tripped anything.
//
// It was a restatement of the rule in the commit message, dressed as a
// test. That is a worse failure than a gate watching the wrong shape,
// because there was no shape at all — nothing about it could ever have gone
// red for a change to the code it named.
//
// This drives `Run`. The fake advances its own head AT THE MOMENT THE LOCK
// IS TAKEN, which is exactly the interleaving: another run finished and
// released while this one queued, so what was pending at plan time is
// already applied by the time we may act.
func TestApply_SkipsWhatAnotherRunAppliedWhileWeQueued(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	tg := tgts(tgt("main", m.GetId(), m.GetContentSha256()))

	fake := &headAdvancingFake{advanceTo: m.GetId()}
	if err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fake.applied > 0 {
		t.Errorf("applied %d migration(s) that another run had already applied.\n\n"+
			"Plan read the head before any lock existed; the lock was taken later. Without "+
			"re-reading the head under the lock, the lock protects a decision made against "+
			"a stale answer (T3-7 pass #14, D14-2).", fake.applied)
	}
}

// headAdvancingFake is the interleaving in one type: the head is empty when
// the plan reads it and equals `advanceTo` from the moment the run lock is
// acquired.
type headAdvancingFake struct {
	covFake
	advanceTo string
	locked    bool
	applied   int
}

func (f *headAdvancingFake) AppliedHead(context.Context) (string, error) {
	if f.locked {
		return f.advanceTo, nil
	}
	return "", nil
}

func (f *headAdvancingFake) AcquireRunLock(context.Context) (RunLock, error) {
	f.locked = true
	return noopHeld{}, nil
}

func (f *headAdvancingFake) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	f.applied++
	return f.covFake.Apply(ctx, m)
}

type noopHeld struct{}

func (noopHeld) Release(context.Context) error { return nil }

// TestRunRollback_ReVerifiesTheHeadUnderTheLock — the SECOND member of
// D14-2's class, which the finder missed and the verifier named.
//
// ⚠️ The property is NOT "lock before planning". Planning first is fine and
// is what both paths do; what matters is that the decision is re-validated
// once the lock is held. The first version of this test asserted the
// ordering instead, and would have demanded a restructure that buys
// nothing — the second version of a check I wrote in the same hour as the
// fix, which is how these keep going wrong.
//
// It also passed VACUOUSLY at first: it searched this function's body for
// the literal "AppliedHead", and the head is read inside `PlanRollback`, so
// there was nothing to find and nothing to fail. Caught by checking why it
// was green rather than by trusting that it was.
//
// Source-level rather than behavioural because the rollback loop has not
// been extracted the way the apply loop was; this records the requirement
// so the next edit cannot quietly drop it.
func TestRunRollback_ReVerifiesTheHeadUnderTheLock(t *testing.T) {
	src := readOrchestratorSource(t)
	i := indexOfFunc(src, "func RunRollback(")
	if i < 0 {
		t.Skip("RunRollback no longer exists — re-read this gate's premise before deleting it")
	}
	body := src[i:]
	if j := indexOfFunc(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	lockAt := indexOfFunc(body, "newRunApplierCache")
	if lockAt < 0 {
		t.Fatal("RunRollback no longer builds an applier cache — this gate cannot tell " +
			"where its run lock is taken")
	}
	// AFTER the cache, which is where the lock is acquired. A read before it
	// is the planner's, and the planner ran unlocked.
	if indexOfFunc(body[lockAt:], "AppliedHead") < 0 {
		t.Error("RunRollback never re-reads AppliedHead after taking the run lock.\n\n" +
			"`PlanRollback` chose what to undo from a head read while no lock was held, so " +
			"another run can apply or roll back in between and this one then undoes a " +
			"migration chosen from a state nobody holds any more. The apply path re-reads " +
			"and SKIPS; this path has to re-read and REFUSE, because a rollback whose head " +
			"moved is being asked to undo something other than what it planned against " +
			"(T3-7 pass #14, D14-2).")
	}
}

func readOrchestratorSource(t *testing.T) string {
	t.Helper()
	b, err := readFileForTest("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	return string(b)
}

func indexOfFunc(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func readFileForTest(name string) ([]byte, error) { return os.ReadFile(name) }

// TestRollback_RefusesOnlyWhenTheHeadActuallyMoved — T3-7 pass #15,
// `B15-4`, and the behavioural half `C15-10` said this file was missing.
//
// Two properties, and the first version of the fix had them the wrong way
// round. It compared the head under the lock against the FIRST PLANNED ID,
// which differs from the head by design: `AppliedHead` deliberately
// EXCLUDES a PhasePending migration, and `PlanRollback` adds the pending
// ones above the head and rolls them back first. So the refusal fired on
// every such cleanup, permanently, blaming a concurrent run that never
// existed.
//
// The question is not "is the head the id we are about to undo" but "is the
// database still where the planner found it". Both directions are driven
// here, because a fix that only stopped refusing would delete the
// protection instead of correcting it.
func TestRollback_RefusesOnlyWhenTheHeadActuallyMoved(t *testing.T) {
	m1 := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	m2 := signedMig(t, "ts-2", "main", "CREATE TABLE y;")
	dir := seedMigs(t, m1, m2)
	tg := tgts(tgt("main", "", ""))

	// ⚠️ The pending-above shape, not just any rollback. `AppliedHead`
	// EXCLUDES a PhasePending migration and `PlanRollback` rolls those back
	// FIRST, so the head and the first planned id differ BY DESIGN — which
	// is the state the old comparison refused forever.
	//
	// The first version of this subtest had one migration, so head and first
	// id were equal and the pre-fix comparison passed too: it could not tell
	// the fix from the defect. Measured, by restoring the old line and
	// watching it stay green.
	t.Run("head unchanged, pending above — the rollback proceeds", func(t *testing.T) {
		fake := &rollbackHeadFake{head: "ts-1", pending: map[string]bool{"ts-2": true}}
		if err := RunRollback(context.Background(), RollbackConfig{
			Targets: tg, MigrationsDir: dir, ToMigrationID: "",
			ApplierFor: func(string) (Applier, error) { return fake, nil },
		}); err != nil {
			t.Fatalf("refused a rollback whose head never moved: %v\n\n"+
				"This is the shape that wedged the PhasePending cleanup: the planner "+
				"chooses ids the head is not equal to, by design.", err)
		}
		if fake.rolledBack == 0 {
			t.Error("nothing was rolled back")
		}
	})

	t.Run("head moved under the lock — refused", func(t *testing.T) {
		// Planned against ts-1; by the time the lock is held another run has
		// advanced the database somewhere else.
		fake := &rollbackHeadFake{head: "ts-1", pending: map[string]bool{"ts-2": true}, headAfterLock: "ts-9"}
		err := RunRollback(context.Background(), RollbackConfig{
			Targets: tg, MigrationsDir: dir, ToMigrationID: "",
			ApplierFor: func(string) (Applier, error) { return fake, nil },
		})
		if err == nil {
			t.Fatal("rolled back against a database another run had moved — the plan no " +
				"longer describes it, and undoing from a state nobody holds is worse " +
				"than stopping")
		}
		if fake.rolledBack != 0 {
			t.Errorf("refused, but only after rolling back %d migration(s)", fake.rolledBack)
		}
	})
}

// rollbackHeadFake reports one head to the planner and, once the run lock is
// taken, another — the interleaving the re-check exists for.
type rollbackHeadFake struct {
	covFake
	head          string
	pending       map[string]bool
	headAfterLock string
	locked        bool
	rolledBack    int
}

// MigrationPhase makes this applier Resumable, which is what lets
// PlanRollback see a PhasePending migration ABOVE the head — the shape the
// old comparison refused forever.
func (f *rollbackHeadFake) MigrationPhase(_ context.Context, id string) (Phase, error) {
	if f.pending[id] {
		return PhasePending, nil
	}
	return PhaseComplete, nil
}

func (f *rollbackHeadFake) ApplyPostTx(context.Context, *applyfetchpb.Migration) error { return nil }

func (f *rollbackHeadFake) AppliedHead(context.Context) (string, error) {
	if f.locked && f.headAfterLock != "" {
		return f.headAfterLock, nil
	}
	return f.head, nil
}

func (f *rollbackHeadFake) AcquireRunLock(context.Context) (RunLock, error) {
	f.locked = true
	return noopHeld{}, nil
}

func (f *rollbackHeadFake) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	f.rolledBack++
	return f.covFake.Rollback(ctx, m)
}

// TestApply_RefusesWhenTheHeadMovedBACKWARD — T3-7 pass #15, `A15-1`.
//
// The post-lock re-read covered one direction. If another run ADVANCED the
// head, this one skips what it already applied — right. If another run moved
// the head BACKWARD, by rolling back while this one queued, nothing noticed:
// the forward skip does not fire and the plan proceeds against a database
// that is no longer where it was planned from.
//
// The sharp member is a squash baseline. `Pending.Adopt` is decided at plan
// time from the pre-lock head — "this database already sits at a migration
// the baseline replaces, so record it and run no DDL". After a concurrent
// rollback that is false, and the adopt branch skips the drift gate too, so
// `adopt_sql` writes "everything up to here is applied" onto a database that
// no longer holds it. Silent, permanent ledger/schema divergence, and the
// run reports success.
//
// Proven by a verifier against production `migrate.Run` before being fixed:
// head ts-2 pre-lock, ts-1 post-lock, a baseline superseding both — adopt_sql
// executed and the run returned nil.
//
// Refusing a BACKWARD move closes the adopt member and the ordinary one
// together, and keeps the forward skip, which is a different question.
func TestApply_RefusesWhenTheHeadMovedBACKWARD(t *testing.T) {
	// There has to be something PENDING, or Plan returns empty and the loop
	// this guard lives in never runs — the first version of this fixture
	// pinned the target at the head and measured "nothing pending".
	m2 := signedMig(t, "ts-2", "main", "CREATE TABLE x;")
	m3 := signedMig(t, "ts-3", "main", "CREATE TABLE y;")
	dir := seedMigs(t, m2, m3)
	tg := tgts(tgt("main", m3.GetId(), m3.GetContentSha256()))

	// Planned when ts-2 was the head, so ts-3 is pending. By the time the
	// lock is held another run has rolled back to ts-1.
	fake := &headRetreatingFake{headBeforeLock: "ts-2", headAfterLock: "ts-1"}
	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	})
	if err == nil {
		t.Fatalf("applied against a database another run had rolled back under us — "+
			"the plan was made from head %q and the database is at %q, so nothing it "+
			"decided still holds", fake.headBeforeLock, fake.headAfterLock)
	}
	if fake.applied > 0 {
		t.Errorf("refused, but only after applying %d migration(s)", fake.applied)
	}
}

type headRetreatingFake struct {
	covFake
	headBeforeLock string
	headAfterLock  string
	locked         bool
	applied        int
}

func (f *headRetreatingFake) AppliedHead(context.Context) (string, error) {
	if f.locked {
		return f.headAfterLock, nil
	}
	return f.headBeforeLock, nil
}

func (f *headRetreatingFake) AcquireRunLock(context.Context) (RunLock, error) {
	f.locked = true
	return noopHeld{}, nil
}

func (f *headRetreatingFake) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	f.applied++
	return f.covFake.Apply(ctx, m)
}
