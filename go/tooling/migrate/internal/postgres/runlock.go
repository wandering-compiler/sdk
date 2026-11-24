package postgres

import (
	"context"
	"fmt"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

var _ migrate.RunLockCapable = (*Applier)(nil)

// The run lock is a session-level advisory lock in the two-key form, so
// pg_locks shows it as (classid, objid) and Check can find it. Advisory
// locks are scoped to the current database, so each database has its own.
// The key is distinct from the post-transaction skirt's lock.
const (
	runLockClass int32 = 0x77313720 // "w17 "
	runLockObj   int32 = 0x72756e00 // "run"
)

// AcquireRunLock implements migrate.RunLockCapable for Postgres.
//
// The transactional body does keep two racing runs from applying a
// migration twice, which is what this package used to rely on — but not by
// failing on the ledger key. The body creates its objects before it
// reaches the ledger INSERT, so the loser fails first on the catalog
// (`duplicate key value violates unique constraint
// "pg_type_typname_nsp_index"`): an error that reads as a broken migration
// rather than as another run in progress. With N replicas migrating at
// boot that is N-1 alarming failures per deploy. With the lock the losers
// wait for it (the orchestrator retries, see migrate.Config.LockWait),
// then find the work done — as on every other dialect.
//
// It is held on the applier's own connection — the one that runs the
// bodies — so it lives exactly as long as the run's session and is freed
// by the server if the process dies.
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	var got bool
	if err := a.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, runLockClass, runLockObj).Scan(&got); err != nil {
		return nil, fmt.Errorf("postgres run lock: %w", err)
	}
	if !got {
		return nil, migrate.ErrLockHeld
	}
	return &runLock{a: a}, nil
}

type runLock struct{ a *Applier }

var _ migrate.CheckedRunLock = (*runLock)(nil)

// Release frees the lock. An unlock the session answers with "not held" means
// the lock was taken from a live session under the run, and that is reported
// as a loss.
//
// An unlock that cannot be sent at all is NOT a loss here, unlike on MySQL:
// every statement of a Postgres run — bodies, skirts, ledger writes — goes
// over this very session, so a session that is gone by release time held the
// lock for every write that ran on it (a write after it ended could not have
// run). The lock went with the session; the run it guarded was protected. It
// stays an error, which the orchestrator prints as a warning, so a run that
// applied everything is not reported failed because the connection dropped
// at its very end.
func (l *runLock) Release(ctx context.Context) error {
	var released bool
	if err := l.a.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1, $2)`, runLockClass, runLockObj).Scan(&released); err != nil {
		return fmt.Errorf("postgres run lock: release (the lock is freed when its session ends): %w", err)
	}
	if !released {
		return fmt.Errorf("postgres run lock: %w: this session no longer held it at release", migrate.ErrLockLost)
	}
	return nil
}

// Check reports whether this run's session still holds the lock.
func (l *runLock) Check(ctx context.Context) error {
	var held bool
	err := l.a.conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
		  AND classid = $1::int4::oid AND objid = $2::int4::oid AND objsubid = 2)`, runLockClass, runLockObj).Scan(&held)
	if err != nil {
		return fmt.Errorf("postgres run lock: %w: the lock session could not be asked whether it still holds the lock: %w", migrate.ErrLockLost, err)
	}
	if !held {
		return fmt.Errorf("postgres run lock: %w: this run's session no longer holds it", migrate.ErrLockLost)
	}
	return nil
}
