package distx_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/wandering-compiler/sdk/go/lib/dbsession"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

func liveServerDSN(t *testing.T, env string) string {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatalf("W17_REQUIRE_LIVE_MIGRATOR=1 but %s is not set — refusing to skip-green", env)
		}
		t.Skipf("%s not set — live test skipped", env)
	}
	return dsn
}

func openLivePG(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbsession.ApplyPGDSNParams(liveServerDSN(t, "W17_PG_DSN")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openLiveMySQL(t *testing.T) *sql.DB {
	t.Helper()
	u, err := url.Parse(liveServerDSN(t, "W17_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mysqldrv.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	db, err := sql.Open("mysql", dbsession.ApplyMySQLDSNParams(cfg.FormatDSN()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The level a caller asks Begin for is the level the adopted transaction
// runs at, on both servers. Under READ COMMITTED a concurrent commit is
// visible to the next read; under REPEATABLE READ it is not. SERIALIZABLE
// shows differently per engine: PostgreSQL reads a stable snapshot, MySQL
// turns the read into a locking one, so the concurrent write blocks.
func TestBegin_IsolationIsHonoured_Live(t *testing.T) {
	for _, dialect := range []struct {
		name         string
		open         func(*testing.T) *sql.DB
		lockTimeout  string
		serializable string
	}{
		{"postgres", openLivePG, "SET lock_timeout = '1s'", "stale"},
		{"mysql", openLiveMySQL, "SET SESSION innodb_lock_wait_timeout = 1", "blocked"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			db := dialect.open(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			table := fmt.Sprintf("iso_%d", time.Now().UnixNano())
			if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (v INT)"); err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE "+table) })
			if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1)"); err != nil {
				t.Fatalf("insert: %v", err)
			}

			reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
			srv := distx.NewServer(reg)
			for _, c := range []struct {
				iso  distxpb.Isolation
				want string
			}{
				{distxpb.Isolation_READ_COMMITTED, "sees"},
				{distxpb.Isolation_REPEATABLE_READ, "stale"},
				{distxpb.Isolation_SERIALIZABLE, dialect.serializable},
			} {
				t.Run(c.iso.String(), func(t *testing.T) {
					resp, err := srv.Begin(ctx, &distxpb.BeginRequest{ConnectionName: "main", Isolation: c.iso})
					if err != nil {
						t.Fatalf("Begin: %v", err)
					}
					defer func() { _, _ = srv.Rollback(context.Background(), &distxpb.RollbackRequest{TxId: resp.GetTxId()}) }()
					tx, release, err := reg.LookupTx(ctx, resp.GetTxId(), "main")
					if err != nil {
						t.Fatalf("LookupTx: %v", err)
					}
					defer release()

					var before int
					if err := tx.QueryRowContext(ctx, "SELECT v FROM "+table).Scan(&before); err != nil {
						t.Fatalf("first read: %v", err)
					}
					// The concurrent writer: its own session, giving up on a
					// lock after a second.
					other, err := db.Conn(ctx)
					if err != nil {
						t.Fatalf("conn: %v", err)
					}
					defer func() { _ = other.Close() }()
					if _, err := other.ExecContext(ctx, dialect.lockTimeout); err != nil {
						t.Fatalf("lock timeout: %v", err)
					}
					got := "blocked"
					if _, err := other.ExecContext(ctx, "UPDATE "+table+" SET v = v + 1"); err == nil {
						var after int
						if err := tx.QueryRowContext(ctx, "SELECT v FROM "+table).Scan(&after); err != nil {
							t.Fatalf("second read: %v", err)
						}
						got = "stale"
						if after != before {
							got = "sees"
						}
					}
					if got != c.want {
						t.Errorf("%s: a concurrent commit is %s to the transaction, want %s", c.iso, got, c.want)
					}
				})
			}
		})
	}
}
