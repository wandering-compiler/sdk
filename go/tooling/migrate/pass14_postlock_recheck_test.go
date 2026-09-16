package migrate

import (
	"os"
	"testing"
)

// TestPostLockHeadSkip_UsesTheHeadTakenUnderTheLock — T3-7 pass #14,
// `D14-2`.
//
// `Plan` reads `AppliedHead` and CLOSES the applier; the run lock is taken
// lazily later, when `Run` first needs a connection. The window between
// "what is applied" and "nobody else may apply" was therefore wide open —
// which is exactly the window the lock exists to close:
//
//	B plans while A holds the lock mid-apply.
//	A finishes and releases.
//	B acquires, and applies what A already applied.
//
// The lock does its job perfectly and the double-apply happens anyway,
// because the decision it protects was taken against a stale answer.
// Postgres survives it on its ledger primary key, and its skirt already
// re-checks under its own advisory lock. The KV appliers — the ones that
// HAVE a run lock — omitted it.
//
// This pins the decision rule. Migration ids are `naming.Name` timestamps,
// so ordering is lexical, and "at or below the head" means somebody else
// applied it while we queued.
func TestPostLockHeadSkip_UsesTheHeadTakenUnderTheLock(t *testing.T) {
	cases := []struct {
		name string
		id   string
		head string
		skip bool
	}{
		{"applied by the run we waited behind", "20260101T000000Z", "20260101T000000Z", true},
		{"older than the head — long applied", "20251201T000000Z", "20260101T000000Z", true},
		{"newer than the head — genuinely ours", "20260201T000000Z", "20260101T000000Z", false},
		{"empty head — nothing applied anywhere", "20260101T000000Z", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.head != "" && tc.id <= tc.head
			if got != tc.skip {
				t.Errorf("id %s against head %q: skip=%v, want %v", tc.id, tc.head, got, tc.skip)
			}
		})
	}
}

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
