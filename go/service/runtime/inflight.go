package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
)

// handlerReturnGrace bounds how long the component waits, after the gRPC
// server has stopped, for handlers to return before it runs the shutdown
// hooks. A forced stop cancels every handler's context, so a handler that
// honours it returns well within this; one still running after it is
// abandoned. A variable so tests can shorten it.
var handlerReturnGrace = 5 * time.Second

// inflight tracks the request handlers that are running, by method.
//
// It exists for the forced-stop path. lifecycle.StopGRPC falls back to
// grpc.Server.Stop when the graceful drain overruns, and Stop returns
// without waiting for handlers (waiting is the unbounded wait the bound
// exists to cap). The shutdown hooks that follow close the pools and the
// bus those handlers use, so without this a handler still finishing its
// request had its resource closed under it.
type inflight struct {
	mu      sync.Mutex
	next    uint64
	active  map[uint64]string
	changed chan struct{}
}

func newInflight() *inflight {
	return &inflight{active: map[uint64]string{}, changed: make(chan struct{}, 1)}
}

// begin records a handler for method and returns its end.
func (f *inflight) begin(method string) (end func()) {
	f.mu.Lock()
	f.next++
	id := f.next
	f.active[id] = method
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.active, id)
		f.mu.Unlock()
		select {
		case f.changed <- struct{}{}:
		default:
		}
	}
}

// wait blocks until no handler is running or d has passed, and returns the
// methods of the handlers still running then (sorted; nil when none).
func (f *inflight) wait(d time.Duration) []string {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		f.mu.Lock()
		n := len(f.active)
		f.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-f.changed:
		case <-timer.C:
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.active) == 0 {
				return nil
			}
			out := make([]string, 0, len(f.active))
			for _, m := range f.active {
				out = append(out, m)
			}
			sort.Strings(out)
			return out
		}
	}
}

func (f *inflight) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	defer f.begin(info.FullMethod)()
	return handler(ctx, req)
}

func (f *inflight) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	defer f.begin(info.FullMethod)()
	return handler(srv, ss)
}
