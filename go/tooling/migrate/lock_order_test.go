package migrate

import (
	"context"
	"sync"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// sharedLocks is one server's run locks, shared by two runs: try-lock
// semantics, ErrLockHeld while another run holds the connection's lock.
type sharedLocks struct {
	mu   sync.Mutex
	held map[string]bool
	// firstTaken is closed per run when that run took its first lock; a run
	// that took one waits (briefly) for the other run to take one too, so
	// the interleaving "each run holds one connection" happens whenever
	// the two runs' first connections differ.
	first map[string]chan struct{}
}

type orderFake struct {
	covFake
	run, conn string
	locks     *sharedLocks
	other     string
}

func (f *orderFake) AcquireRunLock(context.Context) (RunLock, error) {
	f.locks.mu.Lock()
	if f.locks.held[f.conn] {
		f.locks.mu.Unlock()
		return nil, ErrLockHeld
	}
	f.locks.held[f.conn] = true
	ch := f.locks.first[f.run]
	firstForRun := false
	select {
	case <-ch:
	default:
		close(ch)
		firstForRun = true
	}
	other := f.locks.first[f.other]
	f.locks.mu.Unlock()
	if firstForRun {
		select {
		case <-other:
		case <-time.After(300 * time.Millisecond):
		}
	}
	return orderLock{f}, nil
}

type orderLock struct{ f *orderFake }

func (l orderLock) Release(context.Context) error {
	l.f.locks.mu.Lock()
	defer l.f.locks.mu.Unlock()
	delete(l.f.locks.held, l.f.conn)
	return nil
}

func twoConnMigs(t *testing.T, extOnB bool) (string, []ConnTarget) {
	t.Helper()
	ma := signedMig(t, "ts-1", "a", "CREATE TABLE a;")
	mb := &applyfetchpb.Migration{Id: "ts-1", Connection: "b", UpSql: "CREATE TABLE b;", DownSql: "-- down"}
	if extOnB {
		mb.ManifestJson = `{"required_extensions":["pgcrypto"]}`
	}
	mb.ContentSha256 = ContentHash(mb.GetUpSql(), "", "", mb.GetDownSql(), "", nil, "", mb.GetManifestJson())
	return seedMigs(t, ma, mb), tgts(tgt("a", "ts-1", ma.GetContentSha256()), tgt("b", "ts-1", mb.GetContentSha256()))
}

// Two runs over the same two connections take their run locks in ONE order.
// The extension and schema preflights used to take the locks of the
// connections they probe first, so a run with an extension on b locked b
// then a while a run with none locked a then b: each held one and waited
// for the other until LockWait, and both failed.
func TestRun_TwoRunsTakeTheirRunLocksInOneOrder(t *testing.T) {
	defer func(d time.Duration) { lockRetryEvery = d }(lockRetryEvery)
	lockRetryEvery = 20 * time.Millisecond

	locks := &sharedLocks{held: map[string]bool{}, first: map[string]chan struct{}{"x": make(chan struct{}), "y": make(chan struct{})}}
	run := func(name, other string, extOnB bool) error {
		dir, tg := twoConnMigs(t, extOnB)
		return Run(context.Background(), Config{
			Targets: tg, MigrationsDir: dir, LockWait: 2 * time.Second,
			ApplierFor: func(conn string) (Applier, error) {
				return &orderFake{run: name, conn: conn, locks: locks, other: other}, nil
			},
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- run("x", "y", true) }()
	go func() { errs <- run("y", "x", false) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("a run failed: %v", err)
		}
	}
}
