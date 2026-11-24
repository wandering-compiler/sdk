package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

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
//
// ⚠️ "Freed by the server if the process dies" holds for a process whose
// kernel closes the socket. A holder whose host lost the network is not dead
// to the server: its backend sits idle in recv, and with the server's TCP
// keepalive at the OS default (tcp_keepalives_idle = 0 → about two hours on
// Linux) every other replica's apply waits out LockWait and fails for that
// long. So the lock session asks the SERVER to probe it (the knobs below are
// per-session GUCs: they act on the server's socket to this client, which is
// the end that has to notice — a client-side DSN keepalive would only shorten
// the client's detection of a dead server). They are set on the applier's
// own connection, which is the lock's session, and stay for its lifetime.
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	var version int
	if err := a.conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return nil, fmt.Errorf("postgres run lock: read the server version: %w", err)
	}
	if _, err := a.conn.Exec(ctx, lockSessionKeepalive(version), pgx.QueryExecModeSimpleProtocol); err != nil {
		return nil, fmt.Errorf("postgres run lock: ask the server to probe the lock session: %w", err)
	}
	var got bool
	var pid int32
	if err := a.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2), pg_backend_pid()`, runLockClass, runLockObj).Scan(&got, &pid); err != nil {
		return nil, fmt.Errorf("postgres run lock: %w", err)
	}
	if !got {
		return nil, migrate.ErrLockHeld
	}
	return &runLock{a: a, pid: pid}, nil
}

// lockSessionKeepalive makes the server drop a lock session whose client
// stopped answering within about a minute: keepalive probes after 30 s idle,
// every 10 s, three unanswered close it; tcp_user_timeout bounds data the
// server sent that the client never acknowledged. A Unix-socket session
// ignores all four.
//
// tcp_user_timeout exists from PostgreSQL 12 (server_version_num 120000); an
// older server answers its SET "unrecognized configuration parameter", which
// would refuse every apply. Below 12 it is left out and the three keepalive
// settings (which every supported server has) still bound an idle dead
// holder; only data the server sent into a dead link waits out the OS
// retransmission timeout there.
func lockSessionKeepalive(serverVersionNum int) string {
	set := `SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 3`
	if serverVersionNum >= tcpUserTimeoutSince {
		set += `; SET tcp_user_timeout = 60000`
	}
	return set
}

// tcpUserTimeoutSince is the first server_version_num with tcp_user_timeout.
const tcpUserTimeoutSince = 120000

type runLock struct {
	a *Applier
	// pid is the server process the lock was taken on — this run's session.
	pid int32
}

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
//
// The server process answering is compared with the one the lock was taken
// on: a different one means the connection does not keep one server session
// — a transaction-mode pooler (PgBouncer pool_mode=transaction) hands each
// statement to whichever server connection is free — and a session lock
// cannot work through it. That is refused like any loss, but named as what it
// is, so it does not read as a crashed lock holder on every deploy.
func (l *runLock) Check(ctx context.Context) error {
	var held bool
	var pid int32
	err := l.a.conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
		  AND classid = $1::int4::oid AND objid = $2::int4::oid AND objsubid = 2), pg_backend_pid()`, runLockClass, runLockObj).Scan(&held, &pid)
	if err != nil {
		return fmt.Errorf("postgres run lock: %w: the lock session could not be asked whether it still holds the lock: %w", migrate.ErrLockLost, err)
	}
	if !held && pid != l.pid {
		return fmt.Errorf("postgres run lock: %w: the lock was taken on server process %d and this statement ran on %d — the connection goes through a pooler in transaction mode (PgBouncer pool_mode=transaction, or a managed pooler like it), which cannot carry a session advisory lock; point the migrator's DSN at the database directly or at a session-mode pool", migrate.ErrLockLost, l.pid, pid)
	}
	if !held {
		return fmt.Errorf("postgres run lock: %w: this run's session no longer holds it", migrate.ErrLockLost)
	}
	return nil
}
