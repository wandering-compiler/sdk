package migratecli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// liveDatabase creates a fresh database on W17_PG_DSN's server, gives it
// both domains' tables (so a seed into the wrong store would SUCCEED
// silently instead of failing on a missing table — the worse of the two
// shapes), and returns its DSN; dropped on cleanup.
func liveDatabase(t *testing.T, admin string, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	db := fmt.Sprintf("fixtures_%s_%d", name, time.Now().UnixNano())
	if _, err := a.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, db)
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if _, err := c.Exec(ctx, `CREATE TABLE app_item (id TEXT PRIMARY KEY); CREATE TABLE basic_tenant (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func liveRows(t *testing.T, dsn, table string) []string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, "SELECT id FROM "+table+" ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func liveSeed(table, id string) *applyfetchpb.SeedStmt {
	return &applyfetchpb.SeedStmt{
		Sql:  `INSERT INTO "` + table + `" ("id") VALUES ($1) ON CONFLICT ("id") DO NOTHING`,
		Args: []*structpb.Value{structpb.NewStringValue(id)},
	}
}

// Two domains, one store each, one rendered tree shipped to both binaries —
// the deployed shape. Each binary's `fixtures apply` with no --domain seeds
// its own domain into its own store and leaves the other domain's rows for
// the other binary. Run with a render that names the store and with one that
// predates that (the domain alone decides there), through the real factory
// and the real Postgres seeder.
func TestFixturesApply_EachBinarySeedsOnlyItsOwnStore_Live(t *testing.T) {
	admin := os.Getenv("W17_PG_DSN")
	if admin == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live fixtures test skipped")
	}
	for _, named := range []bool{true, false} {
		t.Run(fmt.Sprintf("render-names-store=%v", named), func(t *testing.T) {
			appDSN, basicDSN := liveDatabase(t, admin, "app"), liveDatabase(t, admin, "basic")
			appConn, basicConn := "", ""
			if named {
				appConn, basicConn = "app-postgres", "basic-postgres"
			}
			dir := seedDir(t,
				migrate.FixtureSeed{Domain: "app", Name: "seed", Connection: appConn,
					Statements: []*applyfetchpb.SeedStmt{liveSeed("app_item", "a1")}},
				migrate.FixtureSeed{Domain: "basic", Name: "seed", Connection: basicConn,
					Statements: []*applyfetchpb.SeedStmt{liveSeed("basic_tenant", "b1")}},
			)
			lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"}, migrate.LockConnection{Name: "basic-postgres"})
			// Each binary sees only its own DSN, as a deployed container does.
			for _, bin := range []struct {
				domain, conn, env, dsn string
			}{
				{"app", "app-postgres", "W17_TARGET_APP_POSTGRES", appDSN},
				{"basic", "basic-postgres", "W17_TARGET_BASIC_POSTGRES", basicDSN},
			} {
				var out strings.Builder
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := Dispatch(ctx, []string{"fixtures", "apply", "--lock", lock, "--fixtures", dir},
					Options{Connections: []string{bin.conn}, Domains: []string{bin.domain}, Getenv: envMap{bin.env: bin.dsn}.get, Out: &out})
				cancel()
				if err != nil {
					t.Fatalf("%s binary: %v\n%s", bin.domain, err, out.String())
				}
			}
			for _, c := range []struct {
				dsn, table string
				want       []string
			}{
				{appDSN, "app_item", []string{"a1"}},
				{appDSN, "basic_tenant", nil},
				{basicDSN, "basic_tenant", []string{"b1"}},
				{basicDSN, "app_item", nil},
			} {
				if got := liveRows(t, c.dsn, c.table); strings.Join(got, ",") != strings.Join(c.want, ",") {
					t.Errorf("%s in the %s store: %v, want %v", c.table, map[string]string{appDSN: "app", basicDSN: "basic"}[c.dsn], got, c.want)
				}
			}
		})
	}
}
