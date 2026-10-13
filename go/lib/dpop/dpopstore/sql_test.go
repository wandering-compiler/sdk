package dpopstore_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopstore"
)

// sqlHarness gives every store its own table, created with EnsureSchema and
// dropped after the test. Expiry is judged by FirstSeen, so time needs no
// moving.
func sqlHarness(db *sql.DB, dialect dpopstore.Dialect) harness {
	return harness{
		newStore: func(t *testing.T, maxPerKey, maxTotal int) dpop.JTIStore {
			table := "w17_dpop_jti_test_" + randomSuffix(t)
			s, err := dpopstore.NewSQLJTIStore(db, dialect, dpopstore.WithSQLTable(table), dpopstore.WithSQLMaxPerKey(maxPerKey), dpopstore.WithSQLMaxEntries(maxTotal), dpopstore.WithSQLPruneEvery(1))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureSchema(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureSchema(context.Background()); err != nil {
				t.Fatalf("EnsureSchema is not idempotent: %v", err)
			}
			t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE "+table) })
			return s
		},
		advance: func(*testing.T, time.Duration) {},
	}
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "jti.db")+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLJTIStore_Contract_SQLite(t *testing.T) {
	runContract(t, sqlHarness(openSQLite(t), dpopstore.SQLite))
}

// Expired rows are swept: after the sweep only live rows remain, so the
// table does not grow with every proof ever seen.
func TestSQLJTIStore_PrunesExpiredRows(t *testing.T) {
	db := openSQLite(t)
	s, err := dpopstore.NewSQLJTIStore(db, dpopstore.SQLite, dpopstore.WithSQLPruneEvery(2))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	rec := func(jti string, at time.Time) {
		t.Helper()
		if _, created, err := s.Record(ctx, "tp\x00"+jti, dpop.JTIRecord{Thumbprint: "tp", ProofHash: jti, FirstSeen: at}, time.Second); err != nil || !created {
			t.Fatalf("Record %s: %v %v", jti, created, err)
		}
	}
	rec("old-1", base)
	rec("old-2", base)
	later := base.Add(time.Minute)
	rec("new-1", later)
	rec("new-2", later) // the second record since the last sweep: sweeps
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dpopstore.DefaultSQLTable).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows after the sweep, want the 2 live ones", n)
	}
	if err := s.Prune(ctx, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dpopstore.DefaultSQLTable).Scan(&n); err != nil || n != 0 {
		t.Errorf("Prune left %d rows (%v)", n, err)
	}
}

// A database that does not answer fails the Record, and a bad configuration
// is refused at construction.
func TestSQLJTIStore_FailuresAndConfig(t *testing.T) {
	db := openSQLite(t)
	s, err := dpopstore.NewSQLJTIStore(db, dpopstore.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	// No table: every Record fails, none is reported created.
	if _, created, err := s.Record(context.Background(), "k", dpop.JTIRecord{Thumbprint: "tp", FirstSeen: time.Now()}, time.Minute); err == nil || created {
		t.Errorf("Record without a table = (%v, %v), want an error", created, err)
	}
	_ = db.Close()
	if _, created, err := s.Record(context.Background(), "k", dpop.JTIRecord{Thumbprint: "tp", FirstSeen: time.Now()}, time.Minute); err == nil || created {
		t.Errorf("Record on a closed database = (%v, %v), want an error", created, err)
	}

	if _, err := dpopstore.NewSQLJTIStore(nil, dpopstore.SQLite); err == nil {
		t.Error("nil database accepted")
	}
	if _, err := dpopstore.NewSQLJTIStore(db, "oracle"); err == nil {
		t.Error("unknown dialect accepted")
	}
	for _, bad := range []string{"Jti", "jti; DROP TABLE x", "1jti", "", strings.Repeat("a", 64)} {
		if _, err := dpopstore.NewSQLJTIStore(db, dpopstore.Postgres, dpopstore.WithSQLTable(bad)); err == nil {
			t.Errorf("table name %q accepted", bad)
		}
	}
}

// liveDSN returns the DSN in env, or skips — failing instead when
// W17_REQUIRE_LIVE_MIGRATOR=1, as the repository's other live tests do.
func liveDSN(t *testing.T, env string) string {
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

func TestSQLJTIStore_Contract_PostgresLive(t *testing.T) {
	db, err := sql.Open("postgres", liveDSN(t, "W17_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runContract(t, sqlHarness(db, dpopstore.Postgres))
}

func TestSQLJTIStore_Contract_MySQLLive(t *testing.T) {
	u, err := url.Parse(liveDSN(t, "W17_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("W17_MYSQL_DSN: %v", err)
	}
	cfg := mysqldrv.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runContract(t, sqlHarness(db, dpopstore.MySQL))

	// CLIENT_FOUND_ROWS changes what an ON DUPLICATE KEY no-op reports as
	// affected; the store reads the row back instead, so it stays correct.
	cfg.ClientFoundRows = true
	found, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = found.Close() })
	t.Run("client found rows", func(t *testing.T) { runContract(t, sqlHarness(found, dpopstore.MySQL)) })
}

// A writer that loses the race between its first read and its insert (another
// replica inserted the same jti in between) must report the winner's record,
// not "created". A BEFORE INSERT trigger plays the other replica, so the race
// is lost every time instead of by chance.
func TestSQLJTIStore_LostRaceReadsTheWinner(t *testing.T) {
	db := openSQLite(t)
	s, err := dpopstore.NewSQLJTIStore(db, dpopstore.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER other_replica BEFORE INSERT ON `+dpopstore.DefaultSQLTable+`
		WHEN NEW.proof_hash = 'loser'
		BEGIN
			INSERT INTO `+dpopstore.DefaultSQLTable+` (key_hash, thumbprint, proof_hash, first_seen, expires_at)
			VALUES (NEW.key_hash, NEW.thumbprint, 'winner', NEW.first_seen - 1, NEW.expires_at);
		END`); err != nil {
		t.Fatal(err)
	}
	got, created, err := s.Record(ctx, "tp\x00j", dpop.JTIRecord{Thumbprint: "tp", ProofHash: "loser", FirstSeen: time.Now()}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if created || got.ProofHash != "winner" {
		t.Fatalf("lost race = (%+v, created %v), want the winner's record and not created", got, created)
	}
}
