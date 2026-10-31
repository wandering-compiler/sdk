package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// bodyDB is a driver whose connections remember the session state a body
// left: a body that turns FOREIGN_KEY_CHECKS off and then fails (a down that
// re-adds a key with the checks off, on a statement after the SET).
type bodyDB struct {
	mu     sync.Mutex
	next   int
	fkOff  map[int]bool
	closed map[int]bool
	ran    []int // the connection each body ran on
}

func (d *bodyDB) Connect(context.Context) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	return &bodyConn{db: d, id: d.next}, nil
}

func (d *bodyDB) Driver() driver.Driver { return nil }

type bodyConn struct {
	db *bodyDB
	id int
}

func (c *bodyConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *bodyConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }
func (c *bodyConn) Close() error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.closed[c.id] = true
	return nil
}

func (c *bodyConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.ran = append(c.db.ran, c.id)
	if c.db.fkOff[c.id] {
		return nil, errors.New("ran on a connection left with FOREIGN_KEY_CHECKS off")
	}
	if strings.Contains(q, "SET FOREIGN_KEY_CHECKS = 0") {
		c.db.fkOff[c.id] = true
	}
	if strings.Contains(q, "boom") {
		return nil, errors.New("boom")
	}
	if strings.Contains(q, "SET FOREIGN_KEY_CHECKS = @w17_fk_checks") {
		delete(c.db.fkOff, c.id)
	}
	return driver.RowsAffected(0), nil
}

// A migration body that fails half-way does not hand its connection back to
// the pool: the next body runs on a fresh one, not on the session the failed
// body left with FOREIGN_KEY_CHECKS off (or a transaction open).
func TestApply_AFailedBodyDoesNotReturnItsSessionToThePool(t *testing.T) {
	d := &bodyDB{fkOff: map[int]bool{}, closed: map[int]bool{}}
	db := sql.OpenDB(d)
	db.SetMaxOpenConns(1) // the pool would hand the same connection back
	defer func() { _ = db.Close() }()
	a := &Applier{db: db}
	ctx := context.Background()

	failing := &applyfetchpb.Migration{DownSql: "SET FOREIGN_KEY_CHECKS = 0;\nALTER TABLE boom;\nSET FOREIGN_KEY_CHECKS = @w17_fk_checks;"}
	if err := a.Rollback(ctx, failing); err == nil {
		t.Fatal("Rollback succeeded over a failing body")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "CREATE TABLE t (id INT);"}); err != nil {
		t.Fatalf("the next body: %v", err)
	}
	if len(d.ran) != 2 || d.ran[0] == d.ran[1] {
		t.Fatalf("bodies ran on connections %v: the failed one was reused", d.ran)
	}
	if !d.closed[d.ran[0]] {
		t.Errorf("the failed body's connection %d was not closed", d.ran[0])
	}
}
