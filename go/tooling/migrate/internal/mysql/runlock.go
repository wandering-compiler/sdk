package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

var _ migrate.RunLockCapable = (*Applier)(nil)

// runLockName is the advisory-lock name every apply run against this
// database contends for. MySQL's GET_LOCK namespace is per-SERVER, not
// per-schema, so the name carries the schema to keep two databases on one
// instance independent — which the shadow-DB matrix relies on.
const runLockName = "w17_migrate_apply"

// AcquireRunLock implements migrate.RunLockCapable for MySQL.
//
// ⚠️ Why MySQL needs this when Postgres and SQLite do not, contrary to what
// `lock.go` said for as long as this package has existed:
//
// The claim was that a transactional dialect serialises itself — the body
// runs in a transaction whose `w17_migrations` INSERT has the migration id
// as its primary key, so a second concurrent run fails loudly on the unique
// violation rather than double-applying. That holds for Postgres and
// SQLite. MySQL implicit-commits on DDL, which ENDS the transaction in the
// middle of the body and drops everything after it into autocommit. Two
// racing runs then both apply the tail durably, and the loser's ledger
// INSERT rejects it afterwards — loud, and too late.
//
// Measured on mysql80 and mysql84 through this applier during T3-7 pass
// #14 (D14-1): the row written after the implicit commit survived the
// "loud" ER 1062, while a no-DDL control rolled back cleanly and Postgres
// held. The exposed shape is the raw-SQL escape hatch — a generated body
// dies at its first non-idempotent DDL before reaching any DML.
//
// GET_LOCK is the right primitive: server-side, advisory, and released
// automatically when the session ends, so a killed process does not strand
// it. Fail-fast with a zero timeout, matching the interface's contract
// (ErrLockHeld immediately rather than blocking).
//
// ⚠️ It is SESSION-scoped, and this applier holds a *sql.DB pool. A
// GET_LOCK issued through the pool would land on an arbitrary connection
// and be released the moment that connection is recycled — the lock would
// appear to be held and silently would not be. So the lock takes a
// DEDICATED connection and keeps it for its own lifetime. Same trap as the
// Postgres session advisory lock, and the same answer.
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("mysql run lock: take a dedicated connection: %w", err)
	}

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", runLockName).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql run lock: GET_LOCK: %w", err)
	}
	// 1 = acquired, 0 = held by somebody else, NULL = an error the server
	// declined to raise. NULL is treated as "not acquired": the alternative
	// is applying while unable to tell whether anyone else is.
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, migrate.ErrLockHeld
	}
	return &runLock{conn: conn}, nil
}

type runLock struct{ conn *sql.Conn }

// Release drops the advisory lock and returns the connection.
//
// Closing the connection alone would also release it — that is MySQL's own
// guarantee and the reason a crashed process strands nothing — but the
// explicit RELEASE_LOCK makes the intent legible in a server-side lock
// listing, and it is the difference between "released" and "released
// eventually, when the pool gets round to it".
func (l *runLock) Release(ctx context.Context) error {
	_, err := l.conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", runLockName)
	if cErr := l.conn.Close(); err == nil {
		err = cErr
	}
	return err
}
