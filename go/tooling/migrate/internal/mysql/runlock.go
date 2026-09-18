package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

var _ migrate.RunLockCapable = (*Applier)(nil)

// runLockPrefix is what an operator sees in
// `performance_schema.metadata_locks`; `runLockNameFor` appends the schema.
const runLockPrefix = "w17_migrate_apply"

// runLockNameFor is the advisory-lock name every apply run against ONE
// database contends for.
//
// MySQL's GET_LOCK namespace is per-SERVER, not per-schema, so the name has
// to carry the schema or two unrelated databases on one instance contend
// for the same lock. That is not a missed lock — it is a FALSE one: a
// legitimate apply aborts with "another migration run is in progress against
// this target", naming a run that is against a different database entirely.
// On the shadow-DB matrix, where one instance per (dialect, version) holds
// every project as a database inside it, that is the ordinary case.
//
// ⚠️ The comment above the old constant said all of this, and the constant
// was the bare prefix — the reasoning was right and never reached the code
// (T3-7 pass #15, A15-2, against pass #14's own fix). The Postgres sibling
// it was modelled on is safe only because PG advisory locks are per-DATABASE;
// copying the shape without the namespace is what produced the gap.
//
// Hashed past the length limit rather than cut, so two long schema names
// cannot collapse onto one lock — the trap `TenantRole` hit one package over
// (T3-7 pass #13, B13-6).
//
// ⚠️ MySQL REFUSES an over-long lock name; it does not truncate. Measured on
// mysql84: 65 characters answers `ERROR 4163 … should not exceed 64
// characters`, 64 succeeds. The first version of this function said
// "truncates" and produced 65 (17 + 1 + 30 + 1 + 16), which turned a false
// contention into a hard boot failure for every uuid project whose
// connection name reached nine characters — a bigger population than the
// defect it was fixing (T3-7 pass #15, against the fix from an hour
// earlier).
//
// The budget is spelled out rather than summed in the head, because getting
// it wrong is silent until an engine says so.
func runLockNameFor(schema string) string {
	name := runLockPrefix + "_" + schema
	if len(name) <= mysqlMaxLockName {
		return name
	}
	// prefix + "_" + head + "_" + hash == mysqlMaxLockName, exactly.
	head := mysqlMaxLockName - len(runLockPrefix) - 2 - lockNameHashLen
	sum := sha256.Sum256([]byte(schema))
	return runLockPrefix + "_" + schema[:head] + "_" + hex.EncodeToString(sum[:])[:lockNameHashLen]
}

const (
	// mysqlMaxLockName is what GET_LOCK accepts. Over it is ER_USER_LOCK_WRONG_NAME
	// (4163), a refusal rather than a truncation.
	mysqlMaxLockName = 64
	// lockNameHashLen leaves 64 bits of discriminator — enough that two
	// schemas sharing a 29-character head do not share a lock.
	lockNameHashLen = 16
)

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

	// The schema this connection is actually pointed at, asked of the
	// server rather than parsed out of the DSN — a DSN can name a database
	// the session later changed, and the lock has to match where the writes
	// go.
	var schema sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql run lock: read the current schema: %w", err)
	}
	if !schema.Valid || schema.String == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql run lock: the connection names no database, so the lock could not be scoped to one")
	}
	name := runLockNameFor(schema.String)

	// Keep the SESSION alive for the length of the run.
	//
	// ⚠️ This connection is touched exactly twice — GET_LOCK here and
	// RELEASE_LOCK at the end — with the entire migration in between, and it
	// is idle for all of it. MySQL reaps an idle session at `wait_timeout`
	// (8h by default, but deployments lower it, and a few minutes is a common
	// setting behind a proxy), and ending the session RELEASES THE LOCK. The
	// run would then continue believing it holds an exclusive lock that a
	// second run is free to take (T3-7 pass #15, B15-15).
	//
	// Raising it on this session only: `SET SESSION` touches nothing else on
	// the server, and the connection is this lock's own for its lifetime.
	// A day is chosen to be longer than any migration anyone should be
	// running and short enough that a leaked session still ages out.
	if _, err := conn.ExecContext(ctx, "SET SESSION wait_timeout = 86400"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql run lock: extend the lock session's idle timeout: %w", err)
	}

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
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
	return &runLock{conn: conn, name: name}, nil
}

type runLock struct {
	conn *sql.Conn
	name string
}

// Release drops the advisory lock and returns the connection.
//
// Closing the connection alone would also release it — that is MySQL's own
// guarantee and the reason a crashed process strands nothing — but the
// explicit RELEASE_LOCK makes the intent legible in a server-side lock
// listing, and it is the difference between "released" and "released
// eventually, when the pool gets round to it".
func (l *runLock) Release(ctx context.Context) error {
	// The RESULT is read, not discarded: RELEASE_LOCK answers 1 when this
	// session held the lock, 0 when another session holds it, and NULL when
	// nobody does. The last two mean the lock was NOT ours for some part of
	// the run — the session was reaped or killed and something else may have
	// acquired it meanwhile — and that is worth saying out loud, because the
	// run it guarded has already finished by now. Discarding the answer made
	// a lost lock indistinguishable from a clean release.
	var released sql.NullInt64
	err := l.conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", l.name).Scan(&released)
	if err == nil && (!released.Valid || released.Int64 != 1) {
		err = fmt.Errorf("mysql run lock: %s was no longer held by this session at release "+
			"(RELEASE_LOCK answered %s) — the lock session was reaped or killed, so part of "+
			"this run was not protected by it", l.name, nullIntString(released))
	}
	if cErr := l.conn.Close(); err == nil {
		err = cErr
	}
	return err
}

func nullIntString(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return strconv.FormatInt(v.Int64, 10)
}
