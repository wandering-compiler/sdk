package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// sessionDB is a database/sql driver that records which pooled connection
// ran each statement and tracks each connection's FOREIGN_KEY_CHECKS, so a
// test can see what a session setting did to the pool.
type sessionDB struct {
	mu     sync.Mutex
	next   int
	log    []string // every statement, in order
	fkOff  map[int]bool
	failOn string // a statement containing this fails
	tables []string
}

func (d *sessionDB) Connect(context.Context) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	return &sessionConn{db: d, id: d.next}, nil
}

func (d *sessionDB) Driver() driver.Driver { return nil }

type sessionConn struct {
	db *sessionDB
	id int
}

func (c *sessionConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *sessionConn) Close() error                        { return nil }
func (c *sessionConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }

func (c *sessionConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.log = append(c.db.log, q)
	switch {
	case c.db.failOn != "" && strings.Contains(q, c.db.failOn):
		return nil, errors.New("boom")
	case q == "SET FOREIGN_KEY_CHECKS=0":
		c.db.fkOff[c.id] = true
	case q == "SET FOREIGN_KEY_CHECKS=1":
		delete(c.db.fkOff, c.id)
	case strings.HasPrefix(q, "DROP TABLE") && !c.db.fkOff[c.id]:
		return nil, errors.New("Cannot drop table referenced by a foreign key constraint")
	}
	return driver.RowsAffected(0), nil
}

func (c *sessionConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return &nameRows{names: append([]string(nil), c.db.tables...)}, nil
}

type nameRows struct{ names []string }

func (r *nameRows) Columns() []string { return []string{"table_name"} }
func (r *nameRows) Close() error      { return nil }
func (r *nameRows) Next(dest []driver.Value) error {
	if len(r.names) == 0 {
		return io.EOF
	}
	dest[0] = r.names[0]
	r.names = r.names[1:]
	return nil
}

func newSessionApplier(d *sessionDB) *Applier {
	d.fkOff = map[int]bool{}
	db := sql.OpenDB(d)
	db.SetMaxOpenConns(4)
	return &Applier{db: db}
}

// FOREIGN_KEY_CHECKS is a session setting, so Wipe disables it, drops every
// table and restores it on ONE connection. Issued through the pool, the drops
// ran where FK checks were still on.
func TestWipe_RunsOnOneSessionAndRestoresFKChecks(t *testing.T) {
	d := &sessionDB{tables: []string{"parent", "child", "we`ird"}}
	a := newSessionApplier(d)
	defer func() { _ = a.db.Close() }()

	// Hold a second connection open so the pool has more than one to hand
	// out.
	ctx := context.Background()
	other, err := a.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	_ = other.Close()

	if !contains(d.log, "DROP TABLE IF EXISTS `we``ird`") {
		t.Errorf("a backtick in a table name is not escaped: %q", d.log)
	}
	if len(d.fkOff) != 0 {
		t.Errorf("connections left with FK checks off: %v", d.fkOff)
	}
}

// A failure partway through still puts the session setting back, so no
// pooled connection is handed to the next caller with FK checks off.
func TestWipe_AFailureStillRestoresFKChecks(t *testing.T) {
	d := &sessionDB{tables: []string{"a", "b"}, failOn: "`b`"}
	a := newSessionApplier(d)
	defer func() { _ = a.db.Close() }()

	if err := a.Wipe(context.Background()); err == nil {
		t.Fatal("Wipe succeeded over a failing DROP")
	}
	if len(d.fkOff) != 0 {
		t.Errorf("a failed Wipe left FK checks off on a pooled connection: %v (log %q)", d.fkOff, d.log)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
