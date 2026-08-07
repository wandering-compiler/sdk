//go:build dockertest

package mysql_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

const (
	myRootPass = "snaptest"
	myDB       = "app"
)

// TestSnapshotter_RoundTrip is the S2 mysql verify: dump → wipe →
// restore → fingerprint equal + data spot-check. Needs the mysqldump /
// mysql client binaries (the Snapshotter shells out) and docker.
// Gated by `//go:build dockertest`; skips if the binaries are absent.
func TestSnapshotter_RoundTrip(t *testing.T) {
	for _, bin := range []string{"mysqldump", "mysql"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH — skipping mysql snapshot round-trip", bin)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	driverDSN, urlDSN := startThrowawayMySQL(ctx, t)
	db, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	seed := []string{
		`CREATE TABLE widget (id BIGINT PRIMARY KEY, name VARCHAR(64) NOT NULL, qty INT DEFAULT 0)`,
		`CREATE INDEX widget_name_idx ON widget (name)`,
		`INSERT INTO widget (id, name, qty) VALUES (1, 'a', 3), (2, 'b', 7)`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	before, err := fingerprint.ExtractMySQL(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}

	snap, err := mysql.NewSnapshotter(urlDSN)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump empty")
	}

	if _, err := db.ExecContext(ctx, `DROP TABLE widget`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	wiped, err := fingerprint.ExtractMySQL(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint wiped: %v", err)
	}
	if wiped.FingerprintHex() == before.FingerprintHex() {
		t.Fatal("wipe did not change the fingerprint — setup is wrong")
	}

	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := fingerprint.ExtractMySQL(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if after.FingerprintHex() != before.FingerprintHex() {
		t.Fatalf("fingerprint mismatch:\n before=%s\n  after=%s", before.FingerprintHex(), after.FingerprintHex())
	}
	var qty int
	if err := db.QueryRowContext(ctx, `SELECT qty FROM widget WHERE id = 2`).Scan(&qty); err != nil {
		t.Fatalf("post-restore read: %v", err)
	}
	if qty != 7 {
		t.Errorf("restored qty = %d, want 7", qty)
	}
}

// startThrowawayMySQL returns (driverDSN, urlDSN): the go-sql-driver
// form for the test's own connection and the mysql:// URL form the
// Snapshotter consumes.
func startThrowawayMySQL(ctx context.Context, t *testing.T) (string, string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P",
		"-e", "MYSQL_ROOT_PASSWORD="+myRootPass,
		"-e", "MYSQL_DATABASE="+myDB,
		"mysql:8",
	).Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "stop", id).Run() })

	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "3306/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	port := first[strings.LastIndex(first, ":")+1:]

	driverDSN := fmt.Sprintf("root:%s@tcp(127.0.0.1:%s)/%s?multiStatements=true", myRootPass, port, myDB)
	urlDSN := fmt.Sprintf("mysql://root:%s@127.0.0.1:%s/%s", myRootPass, port, myDB)

	// MySQL takes a while to initialise; poll until it accepts queries.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		db, err := sql.Open("mysql", driverDSN)
		if err == nil {
			pingErr := db.PingContext(ctx)
			_ = db.Close()
			if pingErr == nil {
				return driverDSN, urlDSN
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mysql not ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
