package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// RunCheck counts what a check selects, refuses what is not a single
// read-only SELECT (the round-5 attacks), runs off the run lock's session, and
// reports a failure by SQLSTATE without the data the server's message quotes.
func TestRunCheck_CountsAndCannotWrite_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE TABLE "c" (id BIGINT PRIMARY KEY, pid BIGINT, s TEXT); INSERT INTO "c" VALUES (1, 7, 'secret-value'), (2, NULL, NULL);`}); err != nil {
		t.Fatal(err)
	}
	lock, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("run lock: %v", err)
	}
	defer func() { _ = lock.Release(context.Background()) }()

	n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, "")
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	if n, err = a.RunCheck(ctx, `SELECT "pid", count(*) FROM "c" WHERE ("pid") IS NOT NULL GROUP BY "pid" HAVING count(*) > 0 LIMIT 1`, ""); err != nil || n != 1 {
		t.Fatalf("row-shape count = %d, %v; want 1", n, err)
	}
	for _, attack := range []string{
		`SELECT pg_advisory_unlock_all()`,
		`SELECT count(*) FROM pg_stat_activity WHERE pg_terminate_backend(pid)`,
		`SELECT pg_notify('w17', 'x')`,
		`SELECT pg_sleep(600)`,
		`WITH d AS (DELETE FROM "c" RETURNING 1) SELECT count(*) FROM d`,
		`SELECT 0; DELETE FROM "c"`,
	} {
		if _, err := a.RunCheck(ctx, attack, ""); err == nil {
			t.Errorf("ran %q", attack)
		}
	}
	if err := lock.(interface{ Check(context.Context) error }).Check(ctx); err != nil {
		t.Fatalf("the run lock did not survive the checks: %v", err)
	}
	if n, err = a.RunCheck(ctx, `SELECT count(*) FROM "c"`, ""); err != nil || n != 2 {
		t.Fatalf("rows after the refused writes: %d, %v", n, err)
	}
	// an expression index's probe calls what the index body calls
	lowerProbe := `SELECT (lower(s)), count(*) FROM "c" WHERE ((lower(s))) IS NOT NULL GROUP BY (lower(s)) HAVING count(*) > 0 LIMIT 1`
	if _, err = a.RunCheck(ctx, lowerProbe, ""); err == nil {
		t.Fatal("lower() ran without a body holding it")
	}
	if n, err = a.RunCheck(ctx, lowerProbe, `CREATE UNIQUE INDEX "c_s" ON "c" ((lower(s)));`); err != nil || n != 1 {
		t.Fatalf("expression probe = %d, %v; want 1", n, err)
	}
	_, err = a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "s"::numeric > 0 LIMIT 1`, "")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "SQLSTATE 22P02") {
		t.Fatalf("a failed cast must report only its SQLSTATE: %v", err)
	}
}

// A role that may not lower temp_file_limit still runs its checks: the limit
// is best-effort, the statement timeout is not.
func TestRunCheck_WithoutTheTempFileLimitPrivilege_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("w17_check_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role, pgx.QueryExecModeSimpleProtocol)
		_ = admin.Close(context.Background())
	})
	if _, err := admin.Exec(ctx, `CREATE TABLE "c" (id BIGINT PRIMARY KEY); INSERT INTO "c" VALUES (1);
		CREATE ROLE `+role+` LOGIN PASSWORD 'p'; GRANT SELECT ON "c" TO `+role+`;`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "p")
	a, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	defer func() { _ = a.Close() }()
	if n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" LIMIT 1`, ""); err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
}

// spillProbe is a uniqueness probe (the planner's shape) over 300k distinct
// values: under a 64kB work_mem its GROUP BY spills several MB to temp files.
const spillProbe = `SELECT "s", count(*) FROM "t" WHERE ("s") IS NOT NULL GROUP BY "s" HAVING count(*) > 1 LIMIT 1`

func spillTable(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, `CREATE TABLE "t" (s TEXT); INSERT INTO "t" SELECT md5(i::text) FROM generate_series(1, 300000) i; ANALYZE "t";`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
}

// As a superuser the check runs under temp_file_limit: a probe spilling past
// it fails with SQLSTATE 53400 and is reported by that code alone, which
// --checks turns into a refusal. The same probe under the default limit
// passes, so the limit — not the probe — is what stopped it.
func TestRunCheck_TempFileLimitBoundsASuperuser_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var super bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(context.Background())
	if !super {
		t.Fatal("W17_PG_DSN must name a superuser for this test")
	}
	spillTable(t, ctx, dsn)

	restore := postgres.SetCheckLimitsForTest("1GB", "64kB")
	if n, err := a.RunCheck(ctx, spillProbe, ""); err != nil || n != 0 {
		restore()
		t.Fatalf("under the default limit: %d, %v; want 0", n, err)
	}
	restore()
	defer postgres.SetCheckLimitsForTest("1MB", "64kB")()
	_, err = a.RunCheck(ctx, spillProbe, "")
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 53400") {
		t.Fatalf("a probe spilling past temp_file_limit: %v; want SQLSTATE 53400", err)
	}
	if strings.Contains(err.Error(), "temporary file") {
		t.Fatalf("the server's message leaked into the report: %v", err)
	}
	// a session whose work_mem would hold the whole GROUP BY in memory: the
	// check lowers it to the cap, so the probe still spills — and stops
	big, err := postgres.New(ctx, dsn+"&work_mem=1GB")
	if err != nil {
		t.Fatalf("New with work_mem=1GB: %v", err)
	}
	defer func() { _ = big.Close() }()
	if _, err := big.RunCheck(ctx, spillProbe, ""); err == nil || !strings.Contains(err.Error(), "SQLSTATE 53400") {
		t.Fatalf("work_mem was not lowered to the check's cap: %v; want SQLSTATE 53400", err)
	}
}

// A role that may not set temp_file_limit runs its checks with the limit a
// superuser set for the role, if any: without one the spilling probe runs
// (bounded by the statement timeout); with ALTER ROLE … SET temp_file_limit
// it fails with 53400 as a superuser's does.
func TestRunCheck_ANonSuperuserIsBoundByItsRoleLimit_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spillTable(t, ctx, dsn)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("w17_spill_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role, pgx.QueryExecModeSimpleProtocol)
		_ = admin.Close(context.Background())
	})
	if _, err := admin.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'p'; GRANT SELECT ON "t" TO `+role+`;`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "p")
	defer postgres.SetCheckLimitsForTest("1MB", "64kB")()
	run := func() error {
		a, err := postgres.New(ctx, u.String())
		if err != nil {
			t.Fatalf("connect as %s: %v", role, err)
		}
		defer func() { _ = a.Close() }()
		_, err = a.RunCheck(ctx, spillProbe, "")
		return err
	}
	if err := run(); err != nil {
		t.Fatalf("without a role limit the check must run (it cannot lower temp_file_limit): %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER ROLE `+role+` SET temp_file_limit = '1MB'`); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil || !strings.Contains(err.Error(), "SQLSTATE 53400") {
		t.Fatalf("under the role's temp_file_limit: %v; want SQLSTATE 53400", err)
	}
}
