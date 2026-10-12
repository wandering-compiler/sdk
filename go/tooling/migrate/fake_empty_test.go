package migrate_test

import (
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// An empty adopt preflight, or an empty adopt_sql, refuses --fake: empty means
// the dialect cannot ask "is this database already there", and recording the
// migration anyway would be the operator's word alone. Nothing runs.
func TestFake_EmptyPreflightOrAdoptSQLIsRefused(t *testing.T) {
	cases := map[string]struct{ preflight, adopt string }{
		"empty preflight": {"", "INSERT INTO ledger VALUES ('x');"},
		"empty adopt_sql": {"SELECT must_hold();", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := &applyfetchpb.Migration{
				Id:                "20260101T000000Z",
				Connection:        "main",
				UpSql:             "CREATE TABLE t(id int);",
				DownSql:           "DROP TABLE t;",
				AdoptSql:          c.adopt,
				AdoptPreflightSql: c.preflight,
			}
			m.ContentSha256 = migrate.ContentHashWithPreflight(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, m.GetAdoptSql(), m.GetAdoptPreflightSql(), "")
			dir := seedDirUnchained(t, m)
			st, err := runFake(dir, m)
			if err == nil || !strings.Contains(err.Error(), "carries no adopt preflight") {
				t.Fatalf("--fake with %s was not refused for it: %v", name, err)
			}
			if n := len(st.Calls()); n != 0 {
				t.Errorf("%d body(ies) executed despite the refusal", n)
			}
		})
	}
}
