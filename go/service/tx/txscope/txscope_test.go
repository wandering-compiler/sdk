package txscope_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/txscope"
)

func TestNoScope(t *testing.T) {
	ctx := context.Background()
	if txscope.From(ctx) != nil || txscope.Active(ctx) {
		t.Fatal("a bare context belongs to a scope")
	}
	if got := txscope.AfterCommit(ctx, func() {}); got != txscope.NoScope {
		t.Fatalf("AfterCommit on a bare context = %v", got)
	}
}

func TestCommitRunsParkedWorkInOrderOnce(t *testing.T) {
	ctx, s := txscope.New(context.Background())
	var got []int
	for i := 0; i < 3; i++ {
		if txscope.AfterCommit(ctx, func() { got = append(got, i) }) != txscope.Deferred {
			t.Fatal("not deferred")
		}
	}
	if len(got) != 0 {
		t.Fatal("ran before commit")
	}
	s.Committed()
	s.Committed() // a second end is a no-op
	s.Discarded() // and cannot un-commit
	if len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("ran %v", got)
	}
	if txscope.Active(ctx) {
		t.Fatal("still active after commit")
	}
	if txscope.AfterCommit(ctx, func() {}) != txscope.AlreadyCommitted {
		t.Fatal("late work after commit is not AlreadyCommitted")
	}
}

func TestDiscardDropsParkedWork(t *testing.T) {
	ctx, s := txscope.New(context.Background())
	ran := false
	txscope.AfterCommit(ctx, func() { ran = true })
	s.Discarded()
	s.Committed() // cannot resurrect
	if ran {
		t.Fatal("work ran after a rollback")
	}
	if txscope.AfterCommit(ctx, func() {}) != txscope.Discarded {
		t.Fatal("late work after rollback is not Discarded")
	}
}

func TestPanickingWorkDoesNotStopTheRestOrEscape(t *testing.T) {
	ctx, s := txscope.New(context.Background())
	var after bool
	txscope.AfterCommit(ctx, func() { panic("one") })
	txscope.AfterCommit(ctx, func() { after = true })
	txscope.AfterCommit(ctx, func() { panic("two") })
	panics := s.Committed()
	if !after {
		t.Fatal("a panic stopped the work after it")
	}
	if len(panics) != 2 || panics[0] != "one" || panics[1] != "two" {
		t.Fatalf("panics = %v", panics)
	}
}

func TestRollbackOnlyIsSticky(t *testing.T) {
	_, s := txscope.New(context.Background())
	if s.RollbackOnly() {
		t.Fatal("fresh scope is rollback-only")
	}
	s.MarkRollbackOnly()
	if !s.RollbackOnly() {
		t.Fatal("mark did not stick")
	}
}

// Work parked concurrently with the commit runs EXACTLY once: either the
// scope runs it (Deferred) or the caller does (AlreadyCommitted). Never both,
// never neither.
func TestConcurrentParkingAndCommitRunEachPieceOfWorkOnce(t *testing.T) {
	for round := 0; round < 200; round++ {
		ctx, s := txscope.New(context.Background())
		const n = 64
		var runs [n]atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn := func() { runs[i].Add(1) }
				if txscope.AfterCommit(ctx, fn) == txscope.AlreadyCommitted {
					fn()
				}
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); <-start; s.Committed() }()
		close(start)
		wg.Wait()
		for i := range runs {
			if got := runs[i].Load(); got != 1 {
				t.Fatalf("round %d: work %d ran %d times", round, i, got)
			}
		}
	}
}

// The same race against a rollback: nothing parked may run, and late work is
// told it was discarded.
func TestConcurrentParkingAndRollbackRunNothing(t *testing.T) {
	for round := 0; round < 200; round++ {
		ctx, s := txscope.New(context.Background())
		var ran atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if txscope.AfterCommit(ctx, func() { ran.Add(1) }) == txscope.AlreadyCommitted {
					t.Error("a rolled-back scope reported AlreadyCommitted")
				}
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); <-start; s.Discarded() }()
		close(start)
		wg.Wait()
		s.Committed()
		if ran.Load() != 0 {
			t.Fatalf("round %d: %d pieces of work ran after a rollback", round, ran.Load())
		}
	}
}
