// Package txscope is the in-process face of a distributed transaction: which
// transaction a context belongs to, what has to happen when it commits, and
// whether something inside it already decided it must not.
//
// It exists for COMPOSITION. A business method that calls another one in the
// same process — through the emit-aware server its registry handed back, so
// the callee's `(w17.event_emit)` still fires — can do so inside its own
// `distx.Run`. Two things then have to hold, and neither did:
//
//   - the callee's own `distx.Run` must JOIN the caller's transaction, not open
//     a second one: a second Begin appended a second `w17-tx-id` to the
//     outgoing metadata, storage adopts the FIRST, so the callee's writes
//     silently landed in the caller's transaction while the callee committed
//     an empty one of its own;
//   - the callee's event must wait for the caller's COMMIT. The emit wrapper
//     announces as soon as the callee returns, and the caller can still roll
//     back afterwards — leaving subscribers acting on a write that never
//     happened.
//
// A leaf package with no imports beyond the standard library, because both
// `distx` (which opens scopes) and `txregistry` (which the emit wrappers ask
// whether to wait) need it, and `distx` already imports `txregistry`.
package txscope

import (
	"context"
	"sync"
)

type ctxKey struct{}

// Scope is one root transaction as seen from the process that opened it.
type Scope struct {
	mu           sync.Mutex
	hooks        []func()
	rollbackOnly bool
	ended        bool
	committed    bool
}

// New returns a context carrying a fresh root scope, and the scope.
func New(ctx context.Context) (context.Context, *Scope) {
	s := &Scope{}
	return context.WithValue(ctx, ctxKey{}, s), s
}

// From returns the scope ctx belongs to, or nil.
func From(ctx context.Context) *Scope {
	s, _ := ctx.Value(ctxKey{}).(*Scope)
	return s
}

// Detach returns ctx with its scope hidden: From answers nil, Active false.
//
// For a call that crosses a PROCESS boundary in meaning while staying in one
// process in fact — a composed binary's in-process gRPC hop between tiers.
// Over the network a context's values do not travel, so a transaction is
// visible to the callee only through its metadata; letting the in-process hop
// carry the scope as well would make a storage handler JOIN the caller's
// transaction in a composed binary and open its own in a split one. The same
// code must not change meaning with the deployment topology.
func Detach(ctx context.Context) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, (*Scope)(nil))
}

// Active reports whether ctx belongs to a scope that has not ended — the
// condition under which a new transaction JOINS instead of beginning.
func Active(ctx context.Context) bool {
	s := From(ctx)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ended
}

// Outcome is what [AfterCommit] did with the work it was given.
type Outcome int

const (
	// NoScope: ctx belongs to no transaction; the caller runs the work now.
	NoScope Outcome = iota
	// Deferred: the work runs when the root transaction commits, and never if
	// it rolls back.
	Deferred
	// AlreadyCommitted: the transaction committed before the work arrived; the
	// caller runs it now.
	AlreadyCommitted
	// Discarded: the transaction rolled back before the work arrived; the
	// work must not run.
	Discarded
)

// AfterCommit parks fn until the transaction ctx belongs to commits.
func AfterCommit(ctx context.Context, fn func()) Outcome {
	s := From(ctx)
	if s == nil || fn == nil {
		return NoScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		if s.committed {
			return AlreadyCommitted
		}
		return Discarded
	}
	s.hooks = append(s.hooks, fn)
	return Deferred
}

// MarkRollbackOnly records that something inside the transaction failed and
// rolled back its part. Without savepoints its writes cannot be undone alone,
// so the root must not commit: a caller that swallowed the callee's error
// would otherwise commit the callee's half-done work.
func (s *Scope) MarkRollbackOnly() {
	s.mu.Lock()
	s.rollbackOnly = true
	s.mu.Unlock()
}

// RollbackOnly reports whether [Scope.MarkRollbackOnly] was called.
func (s *Scope) RollbackOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollbackOnly
}

// Committed ends the scope as committed and runs the parked work, in the
// order it was parked, on the calling goroutine. A panic in one piece of work
// does not stop the rest, and does not escape: the transaction has already
// committed, and a panic unwinding into its caller would read as a failed
// commit. What panicked is returned for the caller to report.
func (s *Scope) Committed() (panics []any) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.ended, s.committed = true, true
	hooks := s.hooks
	s.hooks = nil
	s.mu.Unlock()
	for _, fn := range hooks {
		func() {
			defer func() {
				if p := recover(); p != nil {
					panics = append(panics, p)
				}
			}()
			fn()
		}()
	}
	return panics
}

// Discarded ends the scope as rolled back and drops the parked work.
func (s *Scope) Discarded() {
	s.mu.Lock()
	s.ended = true
	s.hooks = nil
	s.mu.Unlock()
}
