package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// Under --fake the drift gate steps aside: the database being adopted is,
// by definition, not in the state the migration's body expects, and the
// adopt preflight is the check. The same migration against the same
// drifted target is refused without --fake.
func TestFake_SkipsTheDriftGate(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	m.AdoptSql = "INSERT INTO ledger VALUES ('ts-1');"
	m.AdoptPreflightSql = "SELECT must_hold();"
	m.ContentSha256 = migrate.ContentHashWithPreflight(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, m.GetAdoptSql(), m.GetAdoptPreflightSql(), "")
	dir := seedDirUnchained(t, m)
	run := func(fake bool) (*stub.Applier, error) {
		st := stub.New()
		st.Fp = "bbbb2222" // drifted: not what the header expects
		err := migrate.Run(context.Background(), migrate.Config{
			Targets:       targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
			MigrationsDir: dir,
			ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
			Fake:          fake,
			Out:           &strings.Builder{},
		})
		return st, err
	}

	if _, err := run(false); err == nil || !strings.Contains(err.Error(), "not in the state this migration was planned against") {
		t.Fatalf("control without --fake = %v, want the drift refusal", err)
	}
	st, err := run(true)
	if err != nil {
		t.Fatalf("--fake on a drifted target = %v, want the preflight to be the check", err)
	}
	calls := st.Calls()
	if len(calls) != 1 || calls[0].GetUpSql() != m.GetAdoptPreflightSql()+"\n"+m.GetAdoptSql() {
		t.Fatalf("--fake ran %v, want exactly the preflight and adopt SQL", calls)
	}
}
