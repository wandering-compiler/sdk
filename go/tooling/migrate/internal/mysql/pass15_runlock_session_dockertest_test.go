//go:build dockertest

package mysql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// TestRunLock_SurvivesAnIdleServerTimeout — T3-7 pass #15, `B15-15`.
//
// The lock connection is touched exactly twice: GET_LOCK when the run starts
// and RELEASE_LOCK when it ends. The whole migration happens in between, and
// the connection is idle for all of it. MySQL reaps an idle session at
// `wait_timeout`, and ENDING THE SESSION RELEASES THE LOCK — so a long run
// would carry on believing it holds an exclusive lock that a second run is
// free to take.
//
// The default is 8 hours, which no test can wait for. So the server is started
// with `--wait-timeout=5` and the run waits past it: the mechanism is
// identical, only the clock is honest about it.
//
// Two properties, and the second is why the first is not enough: the lock must
// still be HELD after the idle period (a second acquirer must be refused), and
// Release must NOTICE when it was not.
func TestRunLock_SurvivesAnIdleServerTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	urlDSN := startShortTimeoutMySQL(ctx, t)

	a, err := mysql.New(ctx, urlDSN)
	if err != nil {
		t.Fatalf("mysql.New: %v", err)
	}
	lock, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}

	// Well past the server's 5s idle timeout, with the lock connection doing
	// nothing — exactly the shape of a real run.
	time.Sleep(9 * time.Second)

	// A second acquirer must still be refused. This is the property that
	// matters: if the session was reaped, the lock is gone and this succeeds.
	a2, err := mysql.New(ctx, urlDSN)
	if err != nil {
		t.Fatalf("mysql.New (second): %v", err)
	}
	if second, err := a2.AcquireRunLock(ctx); err == nil {
		_ = second.Release(ctx)
		t.Fatal("a second run took the lock while the first still held it — " +
			"the idle lock session was reaped and the lock went with it")
	}

	// And the holder must be able to say it still had it.
	if err := lock.Release(ctx); err != nil {
		t.Errorf("Release reported the lock was not held: %v", err)
	}
}

func startShortTimeoutMySQL(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P",
		"-e", "MYSQL_ROOT_PASSWORD="+myRootPass,
		"-e", "MYSQL_DATABASE="+myDB,
		"mysql:8", "--wait-timeout=5",
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
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:%s)/%s?multiStatements=true", myRootPass, port, myDB)
	urlDSN := fmt.Sprintf("mysql://root:%s@127.0.0.1:%s/%s", myRootPass, port, myDB)

	deadline := time.Now().Add(3 * time.Minute)
	for {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			pingErr := db.PingContext(ctx)
			_ = db.Close()
			if pingErr == nil {
				return urlDSN
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
