package migrate_test

import (
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// The allowlist accepts the planner's probe shapes (the platform renders every
// template against it) and refuses everything else — the round-5 attacks and
// the round-6b bypasses of the old denylist among them.
func TestValidateCheckSQL(t *testing.T) {
	pg, my, lite := migrate.CheckPostgres, migrate.CheckMySQL, migrate.CheckSQLite
	type q struct {
		sql string
		d   migrate.CheckDialect
	}
	ok := []q{
		{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, pg},
		{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL AND "pid" <> 7 AND "tid" IS NOT NULL LIMIT 1;`, pg},
		{`SELECT "v", count(*) FROM "t" WHERE ("v") IS NOT NULL GROUP BY "v" HAVING count(*) > 1 LIMIT 1`, pg},
		{`SELECT count(*) FROM "t" WHERE CASE WHEN "x" = 'NaN'::numeric THEN false ELSE "x" <> floor("x") END LIMIT 1`, pg},
		{`SELECT count(*) FROM "t" WHERE "s" !~ '^a;b$' LIMIT 1`, pg},         // a ; inside a string
		{`SELECT count(*) FROM "t" /* a /* nested */ comment */ LIMIT 1`, pg}, // PG nests comments
		{"SELECT count(*) FROM `c` WHERE `pid` NOT IN (SELECT `id` FROM `p`) LIMIT 1", my},
		{"SELECT count(*) FROM `c` WHERE `s` <> 'it''s \\' ok' LIMIT 1", my}, // MySQL backslash escape
		{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, lite},
	}
	for _, c := range ok {
		if err := migrate.ValidateCheckSQL(c.sql, c.d); err != nil {
			t.Errorf("refused a planner-shaped probe %q: %v", c.sql, err)
		}
	}
	refused := []q{
		// round 5
		{`SELECT 0; COMMIT; DELETE FROM "c"; SELECT 0`, my},
		{`PRAGMA query_only = OFF; DELETE FROM "c"`, lite},
		{`ATTACH '/tmp/x.db' AS x`, lite},
		{`SELECT pg_advisory_unlock_all()`, pg},
		{`SELECT pg_sleep(100)`, pg},
		{`SELECT pg_notify('c', 'x')`, pg},
		{`WITH d AS (DELETE FROM "c" RETURNING 1) SELECT count(*) FROM d`, pg},
		// round 6b: the denylist bypasses
		{`SELECT "pg_terminate_backend"(1)`, pg},                     // quoted function name
		{`SELECT count(*) FROM "c" WHERE "PG_SLEEP"(1) IS NULL`, pg}, // quoted, other case
		{`SELECT pg_sleep /* x */ (100)`, pg},                        // a comment before the (
		{"SELECT pg_sleep -- x\n(100)", pg},                          // a line comment before the (
		{"SELECT 1 # ; DELETE FROM c", pg},                           // # is an operator on PG, not a comment
		{"SELECT 1 --1\nINTO OUTFILE '/tmp/x'", my},                  // --1 is not a comment on MySQL
		{"SELECT LOAD_FILE('/etc/passwd') --1", my},
		{"SELECT count(*) FROM `c` --1 ; DELETE FROM `c`", my}, // --1 hides nothing on MySQL
		{`SELECT 1 /* /* */ ; DELETE FROM "c"`, pg},            // unterminated nested comment
		{`SELECT pg_read_file('/etc/passwd')`, pg},             // a name the denylist missed
		{`SELECT current_setting('x')`, pg},
		{`SELECT query_to_xml('select 1', true, true, '')`, pg},
		{`SELECT lo_get(1)`, pg},
		{`SELECT dblink_connect('x')`, pg},
		{`SELECT SLEEP(100)`, my},
		{`SELECT RELEASE_ALL_LOCKS()`, my},
		{"SELECT /*! 1 */ 0", my},
		{"SELECT /*+ x */ 0", my},
		{`SELECT 1 INTO OUTFILE '/tmp/x'`, my},
		{`SELECT 1 INTO DUMPFILE '/tmp/x'`, my},
		{`SELECT * FROM "c" FOR UPDATE`, pg},
		{`SELECT * FROM "c" FOR SHARE`, pg},
		{`SELECT * FROM "c" LOCK IN SHARE MODE`, my},
		{`SELECT '\'; DELETE FROM "c"; SELECT '`, pg}, // PG: '\' is a whole string
		{`SELECT $$x$$; DELETE FROM "c"`, pg},
		{`SELECT E'\''; DELETE FROM "c"; SELECT '`, pg},
		{`SELECT U&"d\0061ta" FROM "c"`, pg},
		{`SELECT load_extension('x')`, lite},
		{`SELECT 0 FROM [c]`, lite},
		{`DELETE FROM "c"`, pg},
		{`SELECT count(*) FROM "c" LIMIT 1; ROLLBACK`, pg},
		{`SELECT set_config('x', 'y', false)`, pg},
	}
	for _, c := range refused {
		if err := migrate.ValidateCheckSQL(c.sql, c.d); !errors.Is(err, migrate.ErrCheckRefused) {
			t.Errorf("accepted %q (err %v)", c.sql, err)
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
