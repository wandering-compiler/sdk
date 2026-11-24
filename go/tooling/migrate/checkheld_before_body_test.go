package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// scanLosesLock is a run-locked applier whose lock is lost WHILE the drift
// gate scans the schema — a lock session KILLed during a long fingerprint
// read, on a dialect whose scan does not run on the lock's own session
// (MySQL: lock on a dedicated connection, scan on the pool).
type scanLosesLock struct {
	*stub.Applier
	scanned bool
}

func (f *scanLosesLock) Fingerprint(ctx context.Context) (string, error) {
	f.scanned = true
	return f.Applier.Fingerprint(ctx)
}

func (f *scanLosesLock) AcquireRunLock(context.Context) (migrate.RunLock, error) {
	return scanLock{f}, nil
}

type scanLock struct{ f *scanLosesLock }

func (l scanLock) Release(context.Context) error { return nil }
func (l scanLock) Check(context.Context) error {
	if l.f.scanned {
		return fmt.Errorf("%w: the lock session was killed during the scan", migrate.ErrLockLost)
	}
	return nil
}

// The run lock is asked again right before the body, not only before the
// drift scan: a lock lost during the scan must stop the body.
func TestRun_LockLostDuringTheDriftScanStopsTheBody(t *testing.T) {
	m := mkMig("ts-1", "main", hdr("aaaa1111", "CREATE TABLE x;"))
	dir := seedDir(t, m)
	f := &scanLosesLock{Applier: stub.New()}
	f.Fp = "aaaa1111"
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return f, nil },
	})
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Run = %v, want ErrLockLost", err)
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("the body ran (%d call(s)) after the lock was lost during the scan", n)
	}
}
