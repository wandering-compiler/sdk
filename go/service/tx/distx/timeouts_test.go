package distx_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// The bounds of one transaction nest: a finisher waiting for a running
// handler (the finish wait) gives up, and the rollbacks bounded by the same
// wait end, long before the orphan watcher reclaims the transaction. Were
// the finish wait the longer one, a Commit behind a stuck handler would
// still be waiting when the watcher rolled the transaction back under it,
// and the busy refusal a caller retries on (D5.3) would never be seen.
func TestTimeouts_TheFinishWaitIsBelowTheOrphanTimeout(t *testing.T) {
	if txregistry.DefaultFinishWait >= distx.DefaultOrphanTimeout {
		t.Errorf("DefaultFinishWait %s >= DefaultOrphanTimeout %s", txregistry.DefaultFinishWait, distx.DefaultOrphanTimeout)
	}
	if grpcrollback.RollbackWait != txregistry.DefaultFinishWait {
		t.Errorf("grpcrollback.RollbackWait %s != DefaultFinishWait %s", grpcrollback.RollbackWait, txregistry.DefaultFinishWait)
	}
	if got := distx.RollbackWait(); got != txregistry.DefaultFinishWait {
		t.Errorf("the client's rollback bound %s != DefaultFinishWait %s", got, txregistry.DefaultFinishWait)
	}
}
