package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// The run lock is a session advisory lock, freed when the server notices its
// session is gone. A holder that lost the network is noticed only by the
// server's own TCP keepalive, which defaults to the OS's (about two hours), so
// the lock session asks the server to probe it. These are per-session server
// settings: what the session reports is what the server's socket uses.
// (The partition itself is proven by partition_probe_test.go, which needs a
// container it can disconnect — see the script beside the audit report.)
func TestRunLock_LockSessionIsProbedByTheServer_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	defer func() { _ = rl.Release(ctx) }()
	for name, want := range map[string]string{
		"tcp_keepalives_idle":     "30",
		"tcp_keepalives_interval": "10",
		"tcp_keepalives_count":    "3",
		"tcp_user_timeout":        "60000",
	} {
		got, err := postgres.SessionSettingForTest(ctx, a, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != want {
			t.Errorf("lock session %s = %s, want %s: a holder that lost the network keeps the lock until the OS keepalive (about two hours) reaps it", name, got, want)
		}
	}
}
