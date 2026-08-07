package grpcx

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

type poolEntry struct {
	conn *grpc.ClientConn
	opts []grpc.DialOption
}

// ScalingConfig controls how ConnectScaled manages multiple connections
// per address.
type ScalingConfig struct {
	// StreamsPerConn is the maximum number of concurrent streams (requests)
	// on a single connection before the pool opens a new one.
	// Default: 100 (matches HTTP/2 MAX_CONCURRENT_STREAMS).
	StreamsPerConn int

	// MaxConns is the maximum number of connections for a single address.
	// When the limit is reached, new requests are routed to the least
	// loaded connection.
	// Default: 1000.
	MaxConns int
}

func (c ScalingConfig) streamsPerConn() int {
	if c.StreamsPerConn <= 0 {
		return 100
	}
	return c.StreamsPerConn
}

func (c ScalingConfig) maxConns() int {
	if c.MaxConns <= 0 {
		return 1000
	}
	return c.MaxConns
}

// scaledEntry represents a single connection within a scaled group.
type scaledEntry struct {
	conn          *grpc.ClientConn
	activeStreams int64 // protected by Pool.mu
}

// scaledGroup holds all connections for a single address managed by
// ConnectScaled.
type scaledGroup struct {
	entries []*scaledEntry
	cfg     ScalingConfig
	opts    []grpc.DialOption
}

// Pool manages a set of gRPC client connections keyed by address.
// It is safe for concurrent use.
//
// Dial options are only used when a connection is first created for a given
// address. Subsequent calls to Connect with the same address return the
// cached connection and ignore any new options.
//
// After Close is called, the pool is no longer usable and Connect will
// return an error.
type Pool struct {
	mu     sync.Mutex
	conns  map[string]*poolEntry
	scaled map[string]*scaledGroup
	closed bool
	stop   chan struct{} // signals health watch to stop
}

// NewPool returns a new empty connection pool.
func NewPool() *Pool {
	return &Pool{
		conns:  make(map[string]*poolEntry),
		scaled: make(map[string]*scaledGroup),
	}
}

// Connect returns an existing connection for the given address or creates a
// new one if none exists. All supplied dial options are passed to grpc.NewClient
// only when a new connection is created; they are ignored on cache hits.
//
// The returned connection is owned by the pool. Callers must not close it
// directly; use Pool.Close to close all connections.
func (p *Pool) Connect(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("grpcx: address cannot be empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("grpcx: pool is closed")
	}

	if entry, ok := p.conns[addr]; ok {
		return entry.conn, nil
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpcx: failed to dial %q: %w", addr, err)
	}

	p.conns[addr] = &poolEntry{conn: conn, opts: opts}
	return conn, nil
}

// ConnectScaled returns a connection for the given address, automatically
// scaling the number of connections based on load. The pool tracks active
// streams on each connection and opens new connections when existing ones
// reach their stream limit (StreamsPerConn), up to MaxConns per address.
//
// The caller MUST call Release after each request/stream completes to
// decrement the active stream counter. Typical usage:
//
//	conn, err := pool.ConnectScaled(addr, cfg, opts...)
//	if err != nil {
//	    return err
//	}
//	defer pool.Release(addr, conn)
//	// ... use conn ...
//
// ScalingConfig and dial options are captured from the first call for a
// given address. Subsequent calls with the same address reuse the stored
// configuration and options.
//
// The returned connection is owned by the pool. Callers must not close it
// directly.
func (p *Pool) ConnectScaled(addr string, cfg ScalingConfig, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("grpcx: address cannot be empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("grpcx: pool is closed")
	}

	group := p.scaled[addr]

	// First connection for this address.
	if group == nil {
		conn, err := grpc.NewClient(addr, opts...)
		if err != nil {
			return nil, fmt.Errorf("grpcx: failed to dial %q: %w", addr, err)
		}
		p.scaled[addr] = &scaledGroup{
			entries: []*scaledEntry{{conn: conn, activeStreams: 1}},
			cfg:     cfg,
			opts:    opts,
		}
		return conn, nil
	}

	spc := int64(group.cfg.streamsPerConn())

	// Find the least loaded connection.
	var best *scaledEntry
	for _, e := range group.entries {
		if best == nil || e.activeStreams < best.activeStreams {
			best = e
		}
	}

	// Least loaded has capacity — use it.
	if best != nil && best.activeStreams < spc {
		best.activeStreams++
		return best.conn, nil
	}

	// All connections are full — create a new one if under the limit.
	if len(group.entries) < group.cfg.maxConns() {
		conn, err := grpc.NewClient(addr, group.opts...)
		if err != nil {
			return nil, fmt.Errorf("grpcx: failed to dial %q: %w", addr, err)
		}
		group.entries = append(group.entries, &scaledEntry{conn: conn, activeStreams: 1})
		return conn, nil
	}

	// At max connections — best effort on the least loaded.
	best.activeStreams++
	return best.conn, nil
}

// Release signals that one stream on the given scaled connection has
// completed. The caller must call Release after each ConnectScaled
// request/stream finishes.
//
// Release is a no-op if the address or connection is not found in the
// scaled pool (e.g. because health watch already removed it). This makes
// it safe to use in defer statements unconditionally.
func (p *Pool) Release(addr string, conn *grpc.ClientConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	group := p.scaled[addr]
	if group == nil {
		return
	}

	for _, e := range group.entries {
		if e.conn == conn {
			if e.activeStreams > 0 {
				e.activeStreams--
			}
			return
		}
	}
}

// Reconnect closes the existing connection for the given address and creates
// a new one using the same dial options. Returns the new connection.
// Returns an error if no connection exists for the address.
//
// For scaled connections, all connections for the address are reconnected
// and their active stream counters are reset to zero.
func (p *Pool) Reconnect(addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("grpcx: pool is closed")
	}

	entry, hasRegular := p.conns[addr]
	group, hasScaled := p.scaled[addr]

	if !hasRegular && !hasScaled {
		return nil, fmt.Errorf("grpcx: no connection for %q", addr)
	}

	// Reconnect scaled connections first (side effect).
	if hasScaled {
		p.reconnectScaledGroupLocked(addr, group)
	}

	// Reconnect regular connection.
	if hasRegular {
		_ = entry.conn.Close()

		conn, err := grpc.NewClient(addr, entry.opts...)
		if err != nil {
			delete(p.conns, addr)
			return nil, fmt.Errorf("grpcx: failed to redial %q: %w", addr, err)
		}

		entry.conn = conn
		return conn, nil
	}

	// Only scaled connections exist — return the first one.
	if len(group.entries) > 0 {
		return group.entries[0].conn, nil
	}

	return nil, fmt.Errorf("grpcx: failed to redial %q: all connections failed", addr)
}

// reconnectScaledGroupLocked reconnects all connections in a scaled group.
// Connections that fail to redial are dropped. If all fail, the group is
// removed from the pool.
//
// Must be called with p.mu held.
func (p *Pool) reconnectScaledGroupLocked(addr string, group *scaledGroup) {
	alive := make([]*scaledEntry, 0, len(group.entries))
	for _, e := range group.entries {
		_ = e.conn.Close()

		conn, err := grpc.NewClient(addr, group.opts...)
		if err != nil {
			continue
		}

		e.conn = conn
		e.activeStreams = 0
		alive = append(alive, e)
	}

	group.entries = alive
	if len(alive) == 0 {
		delete(p.scaled, addr)
	}
}

// EnableHealthWatch starts a background goroutine that periodically checks
// the connectivity state of all connections in the pool. Connections in
// TransientFailure or Shutdown state are closed and reconnected using their
// original dial options. If reconnect fails, the connection is removed from
// the pool.
//
// The goroutine is stopped when Close is called. Calling EnableHealthWatch
// on a pool that is already closed is a no-op — the pool is unusable after
// Close (mirrors Connect/ConnectScaled/Reconnect), so spawning a watcher
// here would leak a goroutine no future Close would ever stop.
func (p *Pool) EnableHealthWatch(interval time.Duration) {
	p.mu.Lock()
	if p.closed || p.stop != nil {
		p.mu.Unlock()
		return
	}
	p.stop = make(chan struct{})
	stop := p.stop
	p.mu.Unlock()

	go p.healthLoop(interval, stop)
}

func (p *Pool) healthLoop(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.checkConnectionsSafe()
		}
	}
}

// checkConnectionsSafe runs one health sweep, recovering any panic so a
// single bad tick neither crashes the process nor silently kills the
// long-lived health-watch goroutine (the loop survives to the next tick).
// No reachable trigger today — the body only calls grpc/map ops on
// non-nil connections — this is defence in depth.
func (p *Pool) checkConnectionsSafe() {
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(context.Background(), fmt.Errorf("PANIC grpcx healthLoop: %v\n%s", r, debug.Stack()))
		}
	}()
	p.checkConnections()
}

func (p *Pool) checkConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for addr, entry := range p.conns {
		state := entry.conn.GetState()
		if state == connectivity.TransientFailure || state == connectivity.Shutdown {
			_ = entry.conn.Close()

			conn, err := grpc.NewClient(addr, entry.opts...)
			if err != nil {
				delete(p.conns, addr)
				continue
			}
			entry.conn = conn
		}
	}

	for addr, group := range p.scaled {
		alive := make([]*scaledEntry, 0, len(group.entries))
		for _, e := range group.entries {
			state := e.conn.GetState()
			if state == connectivity.TransientFailure || state == connectivity.Shutdown {
				_ = e.conn.Close()

				conn, err := grpc.NewClient(addr, group.opts...)
				if err != nil {
					continue
				}

				e.conn = conn
				e.activeStreams = 0
			}
			alive = append(alive, e)
		}

		if len(alive) == 0 {
			delete(p.scaled, addr)
		} else {
			group.entries = alive
		}
	}
}

// Close closes all connections in the pool and stops the health watch
// goroutine if running. After Close, the pool is no longer usable and
// Connect will return an error.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}

	var errs []error
	for addr, entry := range p.conns {
		if err := entry.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("grpcx: failed to close conn %q: %w", addr, err))
		}
	}
	p.conns = make(map[string]*poolEntry)

	for addr, group := range p.scaled {
		for _, e := range group.entries {
			if err := e.conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("grpcx: failed to close scaled conn %q: %w", addr, err))
			}
		}
	}
	p.scaled = make(map[string]*scaledGroup)

	return errors.Join(errs...)
}

// Len returns the number of connections currently in the pool, including
// both regular and scaled connections.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.conns)
	for _, group := range p.scaled {
		n += len(group.entries)
	}
	return n
}
