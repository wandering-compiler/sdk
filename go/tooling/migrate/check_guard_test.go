package migrate_test

import (
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// The validator accepts the planner's probe shapes and refuses everything a
// check could use to leave its read-only SELECT — the round-5 attacks among
// them.
func TestValidateCheckSQL(t *testing.T) {
	ok := []string{
		`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`,
		`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL AND "pid" <> 7 LIMIT 1;`,
		`SELECT "v", count(*) FROM "t" WHERE ("v") IS NOT NULL GROUP BY "v" HAVING count(*) > 1 LIMIT 1`,
		"SELECT count(*) FROM `c` WHERE `pid` NOT IN (SELECT `id` FROM `p`) LIMIT 1",
		`SELECT count(*) FROM "t" WHERE CASE WHEN "x" = 'NaN'::numeric THEN false ELSE "x" <> floor("x") END LIMIT 1`,
		`SELECT count(*) FROM "t" WHERE "s" !~ '^a;b$' LIMIT 1`, // a ; inside a string is not a second statement
	}
	for _, q := range ok {
		if err := migrate.ValidateCheckSQL(q, false); err != nil {
			t.Errorf("refused a planner probe %q: %v", q, err)
		}
	}
	type bad struct {
		q         string
		backslash bool
	}
	refused := []bad{
		{`SELECT 0; COMMIT; DELETE FROM "c"; SELECT 0`, true},
		{`PRAGMA query_only = OFF; DELETE FROM "c"`, false},
		{`SELECT 0; PRAGMA query_only = OFF`, false},
		{`ATTACH '/tmp/x.db' AS x`, false},
		{`SELECT pg_advisory_unlock_all()`, false},
		{`SELECT count(*) FROM "c" WHERE pg_terminate_backend(1)`, false},
		{`SELECT pg_notify('c', 'x')`, false},
		{`SELECT pg_sleep(100)`, false},
		{`SELECT SLEEP(100)`, true},
		{`SELECT RELEASE_ALL_LOCKS()`, true},
		{`SELECT load_file('/etc/passwd')`, true},
		{`SELECT 1 INTO OUTFILE '/tmp/x'`, true},
		{`SELECT * FROM "c" FOR UPDATE`, false},
		{`WITH d AS (DELETE FROM "c" RETURNING 1) SELECT count(*) FROM d`, false},
		{`DELETE FROM "c"`, false},
		{`SELECT set_config('x', 'y', false)`, false},
		{`SELECT '\'; DELETE FROM "c"; SELECT '`, false},   // PG: '\' is a whole string, then a real ;
		{`SELECT $$x$$; DELETE FROM "c"`, false},           // dollar quoting
		{`SELECT E'\''; DELETE FROM "c"; SELECT '`, false}, // prefixed string
		{"SELECT /*! 1; DELETE FROM c */ 0", true},         // MySQL executable comment
		{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1; ROLLBACK`, false},
	}
	for _, b := range refused {
		if err := migrate.ValidateCheckSQL(b.q, b.backslash); !errors.Is(err, migrate.ErrCheckRefused) {
			t.Errorf("accepted %q (err %v)", b.q, err)
		}
	}
}

// A count-shape probe is read as its count; a row-shape probe counts its rows.
func TestCheckCountSQL(t *testing.T) {
	if got := migrate.CheckCountSQL(`SELECT count(*) FROM "c" LIMIT 1;`); got != `SELECT count(*) FROM "c" LIMIT 1` {
		t.Errorf("count shape: %q", got)
	}
	if got := migrate.CheckCountSQL(`SELECT "v", count(*) FROM "t" GROUP BY "v" HAVING count(*) > 1 LIMIT 1`); got != `SELECT count(*) FROM (SELECT "v", count(*) FROM "t" GROUP BY "v" HAVING count(*) > 1 LIMIT 1) AS w17_check` {
		t.Errorf("row shape: %q", got)
	}
}
