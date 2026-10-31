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

// A check may repeat, token for token, a call or a parenthesized group its own
// migration's body contains — an expression index's uniqueness probe repeats
// the expression, bare column names and casts included — and nothing else: not
// a name the body only mentions in a string, a comment or a routine's body,
// not a function the body calls over other arguments, not the body's other
// words, and never a second statement.
func TestValidateMigrationCheckSQL(t *testing.T) {
	pg, my, lite := migrate.CheckPostgres, migrate.CheckMySQL, migrate.CheckSQLite
	const probe = `SELECT (lower(email)), (coalesce(nick, '')), count(*) FROM "u" WHERE ((lower(email)), (coalesce(nick, ''))) IS NOT NULL GROUP BY (lower(email)), (coalesce(nick, '')) HAVING count(*) > 1 LIMIT 1`
	const body = `CREATE UNIQUE INDEX "u_email" ON "u" ((lower(email)), (coalesce(nick, '')));`
	if err := migrate.ValidateMigrationCheckSQL(probe, body, pg); err != nil {
		t.Fatalf("refused the expression index's own probe: %v", err)
	}
	if err := migrate.ValidateCheckSQL(probe, pg); !errors.Is(err, migrate.ErrCheckRefused) {
		t.Fatalf("lower() passed without a body holding it: %v", err)
	}
	// a cast and a bare call (no group of its own) are spans too
	if err := migrate.ValidateMigrationCheckSQL(`SELECT lower((v)::varchar), count(*) FROM "t" GROUP BY lower((v)::varchar) HAVING count(*) > 1 LIMIT 1`,
		`CREATE UNIQUE INDEX t_v ON public.t USING btree (lower((v)::varchar));`, pg); err != nil {
		t.Errorf("cast inside a call: %v", err)
	}
	if err := migrate.ValidateMigrationCheckSQL("SELECT (lower(`email`)), count(*) FROM `u` GROUP BY (lower(`email`)) HAVING count(*) > 1 LIMIT 1",
		"CREATE UNIQUE INDEX `u_email` ON `u` ((lower(`email`)));", my); err != nil {
		t.Errorf("mysql: %v", err)
	}
	if err := migrate.ValidateMigrationCheckSQL(probe, body, lite); err != nil {
		t.Errorf("sqlite: %v", err)
	}

	refused := []struct{ name, sql, body string }{
		{"in neither templates nor body", `SELECT count(*) FROM "u" WHERE pg_read_file('/etc/passwd') IS NOT NULL LIMIT 1`, body},
		{"no body", probe, ""},
		{"the body's function over other arguments", `SELECT count(*) FROM "u" WHERE lower(pg_read_file('/etc/passwd')) IS NOT NULL LIMIT 1`, body},
		{"only in a string", `SELECT (pg_sleep(600))`, `COMMENT ON TABLE "u" IS '(pg_sleep(600))';`},
		{"only in an E string", `SELECT (pg_sleep(600))`, `COMMENT ON TABLE "u" IS E'\' (pg_sleep(600))';`},
		{"only in a comment", `SELECT (pg_sleep(600))`, "-- (pg_sleep(600))\n/* (pg_sleep(1)) */ SELECT 1;"},
		{"only in a routine body", `SELECT (pg_sleep(600))`, `CREATE FUNCTION f() RETURNS void AS $fn$ SELECT (pg_sleep(600)) $fn$ LANGUAGE sql;`},
		{"a quoted name", `SELECT "lower"(email), count(*) FROM "u" GROUP BY 1 HAVING count(*) > 1 LIMIT 1`, body},
		{"a body word outside a span", `WITH d AS (DELETE FROM "u" RETURNING 1) SELECT count(*) FROM d`, `DELETE FROM "u" WHERE (lower(email)) = '';`},
		{"a second statement", `SELECT (lower(email)) FROM "u"; DELETE FROM "u"`, `DELETE FROM "u" WHERE (lower(email)) = '';`},
		{"a statement break inside a span", `SELECT count(*) FROM "u" WHERE x IN (SELECT 1; DELETE FROM "u")`, `CREATE RULE r AS ON INSERT TO v DO ALSO (SELECT 1; DELETE FROM "u");`},
		{"an unbalanced group", `SELECT count(*) FROM "u" WHERE (lower(email)`, body},
	}
	for _, c := range refused {
		if err := migrate.ValidateMigrationCheckSQL(c.sql, c.body, pg); !errors.Is(err, migrate.ErrCheckRefused) {
			t.Errorf("%s: accepted %q (err %v)", c.name, c.sql, err)
		}
	}
	// MySQL: an executable or hint comment in a body is a comment — what it
	// holds is never a span a check may repeat — and so is a # comment
	myBody := "CREATE INDEX i ON t ((upper(x))); /*!80000 SELECT (md5(y)) */ /*+ (sleep(5)) */ SELECT 'x' # (sqrt(z))\n"
	for q, want := range map[string]bool{"SELECT (upper(x))": true, "SELECT (md5(y))": false, "SELECT (sleep(5))": false, "SELECT (sqrt(z))": false} {
		if err := migrate.ValidateMigrationCheckSQL(q, myBody, my); (err == nil) != want {
			t.Errorf("mysql %q: err %v, want accepted=%v", q, err, want)
		}
	}
}

// The body is read exactly as its server reads it, or it admits nothing: a
// span this scanner would see where the server sees a string, a comment, an
// identifier or a longer name is never one a check may repeat.
func TestValidateMigrationCheckSQL_BodyReadAsTheServerReadsIt(t *testing.T) {
	pg, my, lite := migrate.CheckPostgres, migrate.CheckMySQL, migrate.CheckSQLite
	refused := []struct {
		name, sql, body string
		d               migrate.CheckDialect
	}{
		{"pg: a non-ASCII dollar tag", `SELECT (pg_sleep(600))`, `CREATE FUNCTION f() RETURNS void AS $é$ SELECT (pg_sleep(600)) $é$ LANGUAGE sql;`, pg},
		{"pg: a non-ASCII byte starts the name", `SELECT count(*) FROM t WHERE pg_sleep(1) IS NOT NULL LIMIT 1`, `CREATE INDEX i ON t ((épg_sleep(1)));`, pg},
		{"pg: a $ inside the name", `SELECT count(*) FROM t WHERE pg_sleep(1) IS NOT NULL LIMIT 1`, `CREATE INDEX i ON t ((x$pg_sleep(1)));`, pg},
		{"pg: an unterminated dollar quote", `SELECT (lower(x))`, `CREATE INDEX i ON t ((lower(x))); SELECT $q$ never closed`, pg},
		{"mysql: an executable comment", `SELECT (sleep(5))`, `/*!80000 CREATE INDEX i ON t ((sleep(5))) */;`, my},
		{"mysql: a hint comment", `SELECT (sleep(5))`, `SELECT /*+ (sleep(5)) */ 1;`, my},
		{"mysql: a quote inside an executable comment", `SELECT (upper(x))`, "CREATE INDEX i ON t ((upper(x))); /*! '*/ (sleep(5)) ' */", my},
		{"mysql: an identifier starting with digits", `SELECT count(*) FROM t WHERE sleep(5) IS NOT NULL LIMIT 1`, `CREATE INDEX i ON t ((1sleep(5)));`, my},
		{"sqlite: a [bracketed] identifier", `SELECT count(*) FROM t WHERE (randomblob(9)) IS NOT NULL LIMIT 1`, `CREATE INDEX i ON t ([(randomblob(9))]);`, lite},
		{"sqlite: an unterminated [identifier]", `SELECT (lower(x))`, `CREATE INDEX i ON t ((lower(x))); SELECT [never closed`, lite},
		{"an unterminated quote", `SELECT (lower(x))`, `CREATE INDEX i ON t ((lower(x))); SELECT 'x`, pg},
	}
	for _, c := range refused {
		if err := migrate.ValidateMigrationCheckSQL(c.sql, c.body, c.d); !errors.Is(err, migrate.ErrCheckRefused) {
			t.Errorf("%s: accepted %q (err %v)", c.name, c.sql, err)
		}
	}
	accepted := []struct {
		name, sql, body string
		d               migrate.CheckDialect
	}{
		{"sqlite: a [bracketed] table beside the expression", `SELECT (lower(x)), count(*) FROM "t" GROUP BY (lower(x)) HAVING count(*) > 1 LIMIT 1`, `CREATE UNIQUE INDEX i ON [t] ((lower(x)));`, lite},
		{"pg: an ASCII dollar-quoted routine beside the index", `SELECT (lower(x))`, `CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql; CREATE INDEX i ON t ((lower(x)));`, pg},
		{"pg: a non-ASCII name elsewhere", `SELECT (lower(x))`, `COMMENT ON TABLE "t" IS 'é'; CREATE INDEX "é_i" ON t ((lower(x)));`, pg},
	}
	for _, c := range accepted {
		if err := migrate.ValidateMigrationCheckSQL(c.sql, c.body, c.d); err != nil {
			t.Errorf("%s: refused %q: %v", c.name, c.sql, err)
		}
	}
}
