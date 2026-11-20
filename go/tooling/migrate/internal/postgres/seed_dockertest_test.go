package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// A fixture is a SET, and ExecSeed has to treat it as one.
//
// The renderer emits rows in FK order — targets before referrers — so a run
// that stops halfway leaves referrers missing with nothing saying which. That
// is a database in a state no fixture describes, and it is worse than not
// seeding at all: the next thing to read it sees a plausible-looking subset.
//
// Asserted by breaking the LAST statement and then looking for the FIRST one's
// row. Checking that ExecSeed returned an error would pass against a version
// that wrote every statement before the failing one and left them there.
func TestExecSeed_APartialFixtureIsRolledBack(t *testing.T) {
	dsn, ctx := seedTestPG(t)
	ap, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	err = ap.ExecSeed(ctx, []migrate.SeedStmt{
		{SQL: `INSERT INTO seedprobe (id, name) VALUES ($1, $2)`, Args: []any{int64(1), "first"}},
		{SQL: `INSERT INTO no_such_table (id) VALUES ($1)`, Args: []any{int64(2)}},
	})
	if err == nil {
		t.Fatal("a failing statement must fail the seed")
	}
	// The message has to say WHICH statement, or an operator with a
	// hundred-row fixture has nothing to go on.
	if !strings.Contains(err.Error(), "statement 2 of 2") {
		t.Errorf("error should locate the failing statement: %v", err)
	}

	conn := connect(t, ctx, dsn)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM seedprobe`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("seedprobe has %d row(s) — the first statement was left behind by a failed seed", n)
	}
}

// The ordinary path: every statement lands, and the arguments arrive as VALUES
// rather than as interpolated text. A name carrying a quote is the cheapest
// proof of that — it would be a syntax error if anything built SQL by hand.
func TestExecSeed_AppliesParameterizedValues(t *testing.T) {
	dsn, ctx := seedTestPG(t)
	ap, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	const awkward = "O'Brien; DROP TABLE seedprobe;--"
	if err := ap.ExecSeed(ctx, []migrate.SeedStmt{
		{SQL: `INSERT INTO seedprobe (id, name) VALUES ($1, $2)`, Args: []any{int64(1), awkward}},
		{SQL: `INSERT INTO seedprobe (id, name) VALUES ($1, $2)`, Args: []any{int64(2), "plain"}},
	}); err != nil {
		t.Fatalf("ExecSeed: %v", err)
	}

	conn := connect(t, ctx, dsn)
	var got string
	if err := conn.QueryRow(ctx, `SELECT name FROM seedprobe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != awkward {
		t.Errorf("name = %q, want %q — the value was not passed as an argument", got, awkward)
	}
}

// An empty set is not an error. A project with no fixtures for a connection
// runs this and should see nothing happen rather than a refusal.
func TestExecSeed_EmptyIsANoOp(t *testing.T) {
	dsn, ctx := seedTestPG(t)
	ap, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	if err := ap.ExecSeed(ctx, nil); err != nil {
		t.Errorf("an empty seed must be a no-op: %v", err)
	}
}

// seedTestPG starts a throwaway Postgres and creates the probe table. Skips
// when docker is unavailable, matching the redis dockertest in this tree.
func seedTestPG(t *testing.T) (string, context.Context) {
	t.Helper()
	ctx := context.Background()
	// W17_SEED_TEST_DSN: a server to use instead of a container this test
	// starts (a run inside a container, where 127.0.0.1 is not the docker
	// host). Each call gets a database of its own there.
	if base := os.Getenv("W17_SEED_TEST_DSN"); base != "" {
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatalf("W17_SEED_TEST_DSN: %v", err)
		}
		defer func() { _ = admin.Close(ctx) }()
		db := fmt.Sprintf("seed_%d", time.Now().UnixNano())
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatalf("create %s: %v", db, err)
		}
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + db
		dsn := u.String()
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect %s: %v", db, err)
		}
		_, _ = c.Exec(ctx, `CREATE TABLE seedprobe (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`)
		_ = c.Close(ctx)
		return dsn, ctx
	}
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P",
		"-e", "POSTGRES_PASSWORD=seed", "-e", "POSTGRES_DB=seed", "postgres:18").Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "stop", id).Run() })

	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "5432/tcp").Output()
	if err != nil {
		t.Skipf("docker port failed: %v", err)
	}
	hostPort := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		hostPort = "127.0.0.1:" + hostPort[i+1:]
	}
	dsn := "postgres://postgres:seed@" + hostPort + "/seed?sslmode=disable"

	// Postgres accepts connections a moment after the container starts.
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, cerr := pgx.Connect(ctx, dsn)
		if cerr == nil {
			_, _ = c.Exec(ctx, `CREATE TABLE seedprobe (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`)
			_ = c.Close(ctx)
			return dsn, ctx
		}
		if time.Now().After(deadline) {
			t.Skipf("postgres never accepted a connection: %v", cerr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	return c
}

// A fixture whose rows reference another fixture's sorts BEFORE it by name
// (org-hosts before acme-org) and still lands: the seeder
// retries it after the others. A parent no fixture creates still fails,
// naming the fixture; any other error is not retried.
func TestExecSeed_AFixtureReferencingALaterOneLands(t *testing.T) {
	dsn, ctx := seedTestPG(t)
	conn := connect(t, ctx, dsn)
	if _, err := conn.Exec(ctx, `CREATE TABLE org (id BIGINT PRIMARY KEY);
		CREATE TABLE org_host (id BIGINT PRIMARY KEY, org_id BIGINT NOT NULL REFERENCES org (id))`); err != nil {
		t.Fatal(err)
	}
	ap, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ap.Close() })
	host := func(id, org int64) migrate.SeedStmt {
		return migrate.SeedStmt{SQL: `INSERT INTO org_host (id, org_id) VALUES ($1, $2)`, Args: []any{id, org}, File: "app/org-hosts"}
	}
	org := func(id int64) migrate.SeedStmt {
		return migrate.SeedStmt{SQL: `INSERT INTO org (id) VALUES ($1)`, Args: []any{id}, File: "app/acme-org"}
	}
	if err := ap.ExecSeed(ctx, []migrate.SeedStmt{host(1, 7), host(2, 7), org(7)}); err != nil {
		t.Fatalf("a fixture referencing a later one: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM org_host`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("org_host rows = %d (%v), want 2", n, err)
	}

	err = ap.ExecSeed(ctx, []migrate.SeedStmt{host(3, 99), org(8)})
	if err == nil || !strings.Contains(err.Error(), "fixture app/org-hosts") || !strings.Contains(err.Error(), "23503") {
		t.Fatalf("a parent no fixture creates: err = %v, want the fixture named and the FK violation", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM org WHERE id = 8`).Scan(&n); err != nil || n != 0 {
		t.Errorf("org 8 = %d row(s) — a failed seed left a fixture behind", n)
	}

	err = ap.ExecSeed(ctx, []migrate.SeedStmt{
		{SQL: `INSERT INTO org (id) VALUES ($1)`, Args: []any{int64(7)}, File: "app/dup"},
		org(9),
	})
	if err == nil || !strings.Contains(err.Error(), "fixture app/dup") || strings.Contains(err.Error(), "23503") {
		t.Fatalf("a duplicate key is not deferred: err = %v", err)
	}
}
