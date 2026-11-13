package mysql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// T1-1 pass #51, A51-33 — MySQL commits each DDL statement on its own, so a
// migration body that fails after one leaves it committed and the migration
// unrecorded. The failure said nothing of it, and the retry the operator
// reaches for wedges on the committed DDL (1050 "already exists") with the
// bare server error. Both name what happened and the way out.
func TestApply_BodyFailureAfterDDLNamesTheCommittedStatements_Live(t *testing.T) {
	dsn, admin := liveMySQL(t)
	schema := strings.TrimPrefix(mustURL(t, dsn).Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, mysqlMigration("20260101T000000Z", "authors")); err != nil {
		t.Fatalf("first: %v", err)
	}
	m := &applyfetchpb.Migration{
		Id: "20260201T000000Z",
		UpSql: "BEGIN;\nCREATE TABLE `other` (id BIGINT PRIMARY KEY);\nINSERT INTO `missing` VALUES (1);\n" +
			"INSERT INTO w17_migrations VALUES ('20260201T000000Z', 'bb');\nCOMMIT;",
	}
	for try, want := range []string{"1146", "1050"} {
		err := a.Apply(ctx, m)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("try %d: want the server's %s: %v", try+1, want, err)
		}
		for _, says := range []string{"commits each DDL statement on its own", "not recorded", "a retry runs the body from its first statement", "by hand"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("try %d: the failure does not say %q:\n%v", try+1, says, err)
			}
		}
	}
	if !tableExists(t, ctx, admin, schema, "other") {
		t.Fatal("the DDL before the failure was not committed — the premise of the guidance")
	}
}
