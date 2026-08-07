// Package lifecycle bounds process shutdown. It holds the ONE
// implementation of "stop this server, and keep the promise you made
// about how long that takes" — used by the SDK's own gRPC server
// lifecycle, by the reserved SSE channel, and by every transport the
// compiler generates (REST / rpc / admin / MCP).
//
// Two failure modes it exists to prevent, both observed in production:
//
//   - **Unbounded graceful stop.** `grpc.Server.GracefulStop` waits for
//     every in-flight RPC with no limit; `http.Server.Shutdown` waits for
//     every active request until ITS context expires. A long codegen call
//     or a held stream then keeps the process alive past any deployment
//     patience, and the supervisor SIGKILLs it — the drain the graceful
//     stop was supposed to buy is lost anyway, just later and less
//     predictably. [StopGRPC] and [HTTPStopper.Stop] always finish within
//     the timeout, forcing a hard teardown when the drain overruns.
//
//   - **A timeout that is always reached.** A fallback that fires on every
//     shutdown is the same defect from the other side: it adds the full
//     timeout to every deploy. A long-lived stream (SSE / WS) is open by
//     definition — it has nothing to drain and no reason to hold the
//     process — so it must be TOLD to end rather than waited out. That is
//     the drain signal ([Draining]): closed the moment shutdown starts, so
//     a stream handler returns immediately while ordinary in-flight
//     requests keep their full drain budget. A shutdown with no in-flight
//     unary work then completes in milliseconds.
//
// The hard-stop path is a broken promise, so it says so on stderr rather
// than failing the process: the transport DID stop when it was asked to,
// and a non-zero exit for it turns every deploy that had a stream attached
// into a false alarm.
package lifecycle

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
)

// DefaultShutdownTimeout bounds a graceful stop when no operator value is
// configured. It is the contract the SDK's [runtime.Config] has always
// documented; the transports the compiler generates now honour the same
// number instead of waiting forever.
const DefaultShutdownTimeout = 10 * time.Second

// ShutdownTimeoutFromEnv reads `<PREFIX>_SHUTDOWN_TIMEOUT_SECONDS` off
// `lookup` and returns the graceful-stop bound a transport passes to
// [StopGRPC] / [HTTPStopper.Stop]. Unset / unparseable falls back to
// [DefaultShutdownTimeout]; an explicit value <= 0 disables the bound
// (unbounded graceful stop — an operator opt-out, the same `0` semantic
// [runtime.Config].ShutdownTimeout documents).
func ShutdownTimeoutFromEnv(prefix string, lookup func(string) string) time.Duration {
	raw := lookup(prefix + "_SHUTDOWN_TIMEOUT_SECONDS")
	if raw == "" {
		return DefaultShutdownTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultShutdownTimeout
	}
	if n <= 0 {
		return 0 // explicit opt-out — block until every RPC finishes
	}
	return time.Duration(n) * time.Second
}

// grpcStopper is the pair of stop methods [StopGRPC] needs — a seam so
// the bound can be tested without standing up a real server.
type grpcStopper interface {
	GracefulStop()
	Stop()
}

// StopGRPC stops srv gracefully, forcing a hard [grpc.Server.Stop] when
// the drain overruns timeout. A timeout of 0 blocks indefinitely (the
// documented opt-out).
//
// gRPC has no "tell open streams to wind down" hook — GracefulStop sends
// GOAWAY and then waits, and a server-streaming handler pinned on its own
// backend receives no cancellation from it. So for the rpc surface this
// bound IS the drain guarantee: a held stream costs one timeout, once, and
// the process still exits on its own terms rather than by SIGKILL.
//
// The forced path leaves handler goroutines running: grpc.Stop returns
// without waiting for them unless the server was built with
// grpc.WaitForHandlers, which we deliberately do NOT set — it makes Stop
// block until every handler returns, i.e. exactly the unbounded wait this
// function exists to cap. So a caller's post-stop teardown (closing DB
// pools, the bus) can overlap an abandoned handler. That is a broken
// promise, not a design: it is why the forced path is loud, and why the
// timeout should be long enough that reaching it is news.
func StopGRPC(srv *grpc.Server, timeout time.Duration) {
	stopGRPC(srv, timeout)
}

func stopGRPC(srv grpcStopper, timeout time.Duration) {
	if timeout <= 0 {
		srv.GracefulStop()
		return
	}
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown: graceful stop exceeded %s; forcing hard stop (in-flight RPCs aborted)", timeout)
		srv.Stop()
	}
}

// drainKey carries the shutdown-drain channel on a request context.
type drainKey struct{}

// WithDrain attaches a drain channel to ctx. [HTTPStopper] does this for
// every request its server serves; call it directly only when wiring a
// handler outside an [HTTPStopper]-bound server (tests, embedded hosts).
func WithDrain(ctx context.Context, drain <-chan struct{}) context.Context {
	return context.WithValue(ctx, drainKey{}, drain)
}

// Draining returns the channel that closes when the server carrying ctx
// begins shutting down — the signal a long-lived handler (SSE, WS, any
// loop that would otherwise stay open indefinitely) selects on so it ends
// on its own instead of being waited out and then cut.
//
// It returns nil when ctx carries no drain signal, and a receive on a nil
// channel blocks forever — so `case <-lifecycle.Draining(ctx):` is safe in
// a handler that may run outside a bound server, and simply never fires
// there.
//
// Unary handlers must NOT select on this: they are what the drain budget
// exists for, and cutting them at shutdown-start is exactly the
// abort-in-flight-work behaviour the graceful stop is meant to avoid.
func Draining(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(drainKey{}).(<-chan struct{})
	return ch
}

// HTTPStopper bounds one [http.Server]'s shutdown and carries the drain
// signal to its handlers. Build it with [BindHTTP] BEFORE the server
// starts serving.
type HTTPStopper struct {
	srv   *http.Server
	drain chan struct{}

	mu      sync.Mutex
	cancels []context.CancelFunc
	drained bool
}

// BindHTTP installs the drain plumbing on srv and returns the stopper that
// shuts it down. It must be called before srv starts serving — it wires
// srv.BaseContext (so every request context descends from a context this
// package can cancel, and carries the [Draining] signal) and registers the
// drain broadcast as an on-shutdown hook.
//
// An existing srv.BaseContext is composed with, not replaced.
func BindHTTP(srv *http.Server) *HTTPStopper {
	s := &HTTPStopper{srv: srv, drain: make(chan struct{})}
	prev := srv.BaseContext
	srv.BaseContext = func(l net.Listener) context.Context {
		parent := context.Background()
		if prev != nil {
			parent = prev(l)
		}
		ctx, cancel := context.WithCancel(parent) //nolint:gosec // G118: cancel is stored in s.cancels and invoked by every Stop path (and by forceClose); not leaked.
		s.mu.Lock()
		s.cancels = append(s.cancels, cancel)
		s.mu.Unlock()
		return WithDrain(ctx, s.drain)
	}
	// net/http runs the registered hooks in their own goroutines as
	// Shutdown begins — i.e. before it starts waiting on active
	// connections, which is the whole point: the streams get their notice
	// at t=0 of the drain window, not at the end of it.
	srv.RegisterOnShutdown(s.startDrain)
	return s
}

// startDrain closes the drain channel once. Idempotent — Shutdown may be
// called more than once, and Stop's own teardown path also reaches here.
func (s *HTTPStopper) startDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drained {
		return
	}
	s.drained = true
	close(s.drain)
}

// Stop shuts the bound server down within timeout and reports the outcome
// as an error ONLY when the server itself failed — never for the timeout.
//
// The sequence: [http.Server.Shutdown] stops accepting, closes idle
// connections, broadcasts the drain signal and waits for active requests.
// Handlers watching [Draining] return at once, so a server whose only
// long-lived traffic is streams drains in milliseconds; an in-flight unary
// request keeps the whole budget. If the budget runs out anyway, the
// request contexts are cancelled (unwinding handlers blocked on a backend
// call) and the server is force-closed, so the process still exits by the
// deadline it promised.
//
// A timeout of 0 waits indefinitely — the documented opt-out.
func (s *HTTPStopper) Stop(timeout time.Duration) error {
	return s.StopWith(timeout, s.srv.Shutdown)
}

// StopWith is [HTTPStopper.Stop] driven by a caller-supplied graceful
// shutdown — for a server wrapped by a library that has teardown of its
// own to run first (the MCP transport's session sweeper). `shutdown` MUST
// end up calling the bound server's Shutdown with the context it is
// given; everything else about the bound + the forced close is unchanged.
func (s *HTTPStopper) StopWith(timeout time.Duration, shutdown func(context.Context) error) error {
	// Release the per-listener base contexts on EVERY path, not just the
	// forced one: once the server has stopped, nothing derives from them
	// again, and leaving them uncancelled leaks a context per listener for
	// the rest of the process.
	defer s.cancelAll()
	if timeout <= 0 {
		return shutdown(context.Background())
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := shutdown(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Printf("shutdown: graceful drain exceeded %s; forcing close (in-flight requests aborted)", timeout)
	s.forceClose()
	// Deliberately not an error: the transport stopped when it was asked
	// to. Surfacing the overrun as a failure is what turned every deploy
	// with a stream attached into a red one.
	return nil
}

// forceClose cancels every live request context, then closes the server.
// Cancelling first gives a handler blocked on a backend call the chance to
// unwind through its own error path (and its deferred cleanup) instead of
// only noticing when its socket dies under it.
func (s *HTTPStopper) forceClose() {
	s.startDrain()
	s.cancelAll()
	_ = s.srv.Close()
}

// cancelAll cancels every base context handed out so far. Safe to call
// repeatedly — a CancelFunc is idempotent.
func (s *HTTPStopper) cancelAll() {
	s.mu.Lock()
	cancels := append([]context.CancelFunc(nil), s.cancels...)
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
