package migrate_test

import (
	"strings"
	"testing"
)

// The drift refusal used to end "there is no command that records a
// migration as applied without running it". That was true when it was
// written (`migrate adopt` had just been removed); `migrate apply --fake`
// came later and is exactly that command for the "catalog ahead
// of ledger" case this refusal describes — guarded by the console-rendered
// preflight, so it refuses unless the database holds what the migration
// describes. A loud refusal that denies the exit that exists sends the
// operator to a hand-written ledger INSERT, the unchecked act --fake replaced.
func TestDriftRefusal_NamesTheFakeApply(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	err := runOne(t, m, "bbbb2222", nil)
	if err == nil {
		t.Fatal("apply must refuse on a drifted target")
	}
	msg := err.Error()
	if strings.Contains(msg, "no command that records") {
		t.Errorf("the refusal denies a remedy that exists (`migrate apply --fake`):\n%s", msg)
	}
	if !strings.Contains(msg, "migrate apply --fake") {
		t.Errorf("the refusal must name `migrate apply --fake` for the catalog-ahead-of-ledger case:\n%s", msg)
	}
}
