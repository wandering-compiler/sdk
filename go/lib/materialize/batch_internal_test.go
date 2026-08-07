package materialize

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("fake: no real connection")
}
func (fakeConnector) Driver() driver.Driver { return nil }

func TestEffectiveParallel(t *testing.T) {
	db := sql.OpenDB(fakeConnector{})
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(5) // headroom = 5 - 1 = 4

	cases := []struct {
		name        string
		maxParallel int
		pool        *sql.DB
		want        int
	}{
		{"nil pool honoured", 8, nil, 8},
		{"floor at 1", 0, nil, 1},
		{"negative floors at 1", -3, nil, 1},
		{"clamped to pool headroom", 10, db, 4},
		{"under headroom unchanged", 2, db, 2},
		{"exactly headroom unchanged", 4, db, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveParallel(c.maxParallel, c.pool); got != c.want {
				t.Fatalf("effectiveParallel(%d, pool=%v) = %d, want %d", c.maxParallel, c.pool != nil, got, c.want)
			}
		})
	}
}

// MaxOpenConns of 1 leaves headroom floored at 1 (never 0).
func TestEffectiveParallel_SingleConnPool(t *testing.T) {
	db := sql.OpenDB(fakeConnector{})
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if got := effectiveParallel(8, db); got != 1 {
		t.Fatalf("single-conn pool: got %d, want 1", got)
	}
}
