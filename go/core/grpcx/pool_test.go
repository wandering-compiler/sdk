package grpcx

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var insecureOpt = grpc.WithTransportCredentials(insecure.NewCredentials())

func TestNewPool(t *testing.T) {
	p := NewPool()
	if p == nil {
		t.Fatal("NewPool returned nil")
	}
	if p.Len() != 0 {
		t.Fatalf("new pool should be empty, got %d", p.Len())
	}
}

// ---------------------------------------------------------------------------
// Connect
// ---------------------------------------------------------------------------

func TestPool_Connect_EmptyAddr(t *testing.T) {
	p := NewPool()
	_, err := p.Connect("")
	if err == nil {
		t.Fatal("expected error for empty address")
	}
}

func TestPool_Connect_CreatesConnection(t *testing.T) {
	p := NewPool()
	conn, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil connection")
	}
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
	_ = p.Close()
}

func TestPool_Connect_ReusesConnection(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	conn1, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	conn2, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn1 != conn2 {
		t.Fatal("expected same connection for same address")
	}
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
}

func TestPool_Connect_DifferentAddresses(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	conn1, _ := p.Connect("passthrough:///localhost:0", insecureOpt)
	conn2, _ := p.Connect("passthrough:///localhost:1", insecureOpt)
	if conn1 == conn2 {
		t.Fatal("expected different connections for different addresses")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}
}

func TestPool_Connect_StoresOpts(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p.mu.Lock()
	entry := p.conns["passthrough:///localhost:0"]
	p.mu.Unlock()

	if len(entry.opts) != 1 {
		t.Fatalf("expected 1 stored dial option, got %d", len(entry.opts))
	}
}

func TestPool_Connect_AfterClose(t *testing.T) {
	p := NewPool()
	_ = p.Close()

	_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err == nil {
		t.Fatal("expected error when connecting on closed pool")
	}
}

func TestPool_Connect_DialError(t *testing.T) {
	p := NewPool()
	// grpc.NewClient fails when no transport credentials are set.
	_, err := p.Connect("localhost:0")
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
	if p.Len() != 0 {
		t.Fatalf("pool should be empty after dial error, got %d", p.Len())
	}
}

func TestPool_ConcurrentAccess(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestPool_Close_ClearsPool(t *testing.T) {
	p := NewPool()
	_, _ = p.Connect("passthrough:///localhost:0", insecureOpt)

	if err := p.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Len() != 0 {
		t.Fatalf("expected empty pool after close, got %d", p.Len())
	}
}

func TestPool_Close_Error(t *testing.T) {
	p := NewPool()
	conn, _ := p.Connect("passthrough:///localhost:0", insecureOpt)
	// Close the conn directly so Pool.Close will encounter an error.
	_ = conn.Close()

	if err := p.Close(); err == nil {
		t.Fatal("expected error when closing already-closed connection")
	}
	if p.Len() != 0 {
		t.Fatalf("pool should be empty after Close, got %d", p.Len())
	}
}

func TestPool_Close_Empty(t *testing.T) {
	p := NewPool()
	if err := p.Close(); err != nil {
		t.Fatalf("unexpected error closing empty pool: %v", err)
	}
}

func TestPool_Close_ClearsScaledPool(t *testing.T) {
	p := NewPool()
	cfg := ScalingConfig{StreamsPerConn: 2, MaxConns: 3}
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	if err := p.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Len() != 0 {
		t.Fatalf("expected empty pool after close, got %d", p.Len())
	}
}

func TestPool_Close_ScaledError(t *testing.T) {
	p := NewPool()
	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 2}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	// Close the conn directly so Pool.Close will encounter an error.
	_ = conn.Close()

	if err := p.Close(); err == nil {
		t.Fatal("expected error when closing already-closed scaled connection")
	}
}

func TestPool_Close_StopsHealthWatch(t *testing.T) {
	p := NewPool()
	p.EnableHealthWatch(10 * time.Millisecond)
	_ = p.Close()

	// Verify stop channel is nil after close.
	p.mu.Lock()
	stopped := p.stop == nil
	p.mu.Unlock()

	if !stopped {
		t.Fatal("expected health watch to be stopped after Close")
	}
}

// ---------------------------------------------------------------------------
// Reconnect (regular)
// ---------------------------------------------------------------------------

func TestPool_Reconnect(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	conn1, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	conn2, err := p.Reconnect("passthrough:///localhost:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conn1 == conn2 {
		t.Fatal("expected new connection after reconnect")
	}
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
}

func TestPool_Reconnect_UnknownAddr(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, err := p.Reconnect("passthrough:///unknown:0")
	if err == nil {
		t.Fatal("expected error for unknown address")
	}
}

func TestPool_Reconnect_AfterClose(t *testing.T) {
	p := NewPool()
	_, _ = p.Connect("passthrough:///localhost:0", insecureOpt)
	_ = p.Close()

	_, err := p.Reconnect("passthrough:///localhost:0")
	if err == nil {
		t.Fatal("expected error on closed pool")
	}
}

func TestPool_Reconnect_DialError(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Replace opts with invalid ones to force redial failure.
	p.mu.Lock()
	p.conns["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	_, err = p.Reconnect("passthrough:///localhost:0")
	if err == nil {
		t.Fatal("expected error for redial failure")
	}
	if p.Len() != 0 {
		t.Fatalf("expected pool to remove failed entry, got %d", p.Len())
	}
}

func TestPool_Reconnect_Concurrent(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rerr := p.Reconnect("passthrough:///localhost:0")
			if rerr != nil {
				t.Errorf("unexpected error: %v", rerr)
			}
		}()
	}
	wg.Wait()

	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
}

// ---------------------------------------------------------------------------
// Health Watch (regular)
// ---------------------------------------------------------------------------

func TestPool_EnableHealthWatch(t *testing.T) {
	p := NewPool()

	_, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p.EnableHealthWatch(10 * time.Millisecond)
	// Second call should be a no-op.
	p.EnableHealthWatch(10 * time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}

	_ = p.Close()
}

func TestPool_EnableHealthWatch_AfterCloseIsNoop(t *testing.T) {
	p := NewPool()
	_ = p.Close()

	// Enabling the watch on an already-closed pool must not spawn a
	// goroutine that no future Close would ever stop.
	p.EnableHealthWatch(10 * time.Millisecond)

	p.mu.Lock()
	started := p.stop != nil
	p.mu.Unlock()

	if started {
		t.Fatal("EnableHealthWatch on a closed pool must be a no-op (no health-watch goroutine started)")
	}
}

func TestPool_EnableHealthWatch_RemovesFailedConn(t *testing.T) {
	p := NewPool()

	conn, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_ = conn.Close()

	p.checkConnections()

	if p.Len() != 1 {
		t.Fatalf("expected pool size 1 after health check reconnect, got %d", p.Len())
	}

	p.mu.Lock()
	entry := p.conns["passthrough:///localhost:0"]
	p.mu.Unlock()

	if entry.conn == conn {
		t.Fatal("expected new connection after health check reconnect")
	}

	_ = p.Close()
}

func TestPool_EnableHealthWatch_RemovesOnDialError(t *testing.T) {
	p := NewPool()

	conn, err := p.Connect("passthrough:///localhost:0", insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_ = conn.Close()
	p.mu.Lock()
	p.conns["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	p.checkConnections()

	if p.Len() != 0 {
		t.Fatalf("expected empty pool after failed reconnect, got %d", p.Len())
	}

	_ = p.Close()
}

// ---------------------------------------------------------------------------
// ScalingConfig defaults
// ---------------------------------------------------------------------------

func TestScalingConfig_Defaults(t *testing.T) {
	cfg := ScalingConfig{}
	if cfg.streamsPerConn() != 100 {
		t.Fatalf("expected default streamsPerConn 100, got %d", cfg.streamsPerConn())
	}
	if cfg.maxConns() != 1000 {
		t.Fatalf("expected default maxConns 1000, got %d", cfg.maxConns())
	}
}

func TestScalingConfig_Negative(t *testing.T) {
	cfg := ScalingConfig{StreamsPerConn: -1, MaxConns: -5}
	if cfg.streamsPerConn() != 100 {
		t.Fatalf("expected default streamsPerConn 100, got %d", cfg.streamsPerConn())
	}
	if cfg.maxConns() != 1000 {
		t.Fatalf("expected default maxConns 1000, got %d", cfg.maxConns())
	}
}

func TestScalingConfig_Custom(t *testing.T) {
	cfg := ScalingConfig{StreamsPerConn: 50, MaxConns: 5}
	if cfg.streamsPerConn() != 50 {
		t.Fatalf("expected streamsPerConn 50, got %d", cfg.streamsPerConn())
	}
	if cfg.maxConns() != 5 {
		t.Fatalf("expected maxConns 5, got %d", cfg.maxConns())
	}
}

// ---------------------------------------------------------------------------
// ConnectScaled
// ---------------------------------------------------------------------------

func TestPool_ConnectScaled_EmptyAddr(t *testing.T) {
	p := NewPool()
	_, err := p.ConnectScaled("", ScalingConfig{})
	if err == nil {
		t.Fatal("expected error for empty address")
	}
}

func TestPool_ConnectScaled_AfterClose(t *testing.T) {
	p := NewPool()
	_ = p.Close()

	_, err := p.ConnectScaled("passthrough:///localhost:0", ScalingConfig{}, insecureOpt)
	if err == nil {
		t.Fatal("expected error on closed pool")
	}
}

func TestPool_ConnectScaled_DialError(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, err := p.ConnectScaled("localhost:0", ScalingConfig{})
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
	if p.Len() != 0 {
		t.Fatalf("pool should be empty after dial error, got %d", p.Len())
	}
}

func TestPool_ConnectScaled_CreatesFirstConnection(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil connection")
	}
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}
}

func TestPool_ConnectScaled_ReusesBelowThreshold(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 10, MaxConns: 5}
	conn1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	conn2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	if conn1 != conn2 {
		t.Fatal("expected same connection when below stream threshold")
	}
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}

	// Verify active streams incremented correctly.
	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	streams := group.entries[0].activeStreams
	p.mu.Unlock()

	if streams != 2 {
		t.Fatalf("expected 2 active streams, got %d", streams)
	}
}

func TestPool_ConnectScaled_ScalesUpWhenFull(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 2, MaxConns: 3}

	// Fill first connection.
	conn1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	conn2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if conn1 != conn2 {
		t.Fatal("expected same connection below threshold")
	}

	// Third request should trigger a new connection.
	conn3, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if conn3 == conn1 {
		t.Fatal("expected new connection when first is full")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}
}

func TestPool_ConnectScaled_RespectsMaxConns(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 2}

	// Create 2 connections (max).
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	c2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	if c1 == c2 {
		t.Fatal("expected different connections")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}

	// Third request should go to least loaded (both have 1 stream, picks first found).
	c3, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if c3 != c1 && c3 != c2 {
		t.Fatal("expected existing connection when at max")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2 (no new connections), got %d", p.Len())
	}
}

func TestPool_ConnectScaled_LeastLoadedSelection(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 2, MaxConns: 2}

	// First connection: 2 streams (full).
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Second connection: 1 stream.
	c2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if c2 == c1 {
		t.Fatal("expected second connection when first is full")
	}

	// Release one stream on first connection.
	p.Release("passthrough:///localhost:0", c1)

	// Now c1 has 1 stream, c2 has 1 stream. Next request goes to least loaded
	// (both equal, picks first found = c1).
	c3, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if c3 != c1 {
		t.Fatal("expected least loaded connection (first with equal load)")
	}
}

func TestPool_ConnectScaled_DialErrorOnScaleUp(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 3}

	// First connection succeeds.
	_, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Sabotage opts to force dial failure on scale-up.
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	_, err = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if err == nil {
		t.Fatal("expected error on dial failure during scale-up")
	}
}

func TestPool_ConnectScaled_StoresConfigFromFirstCall(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg1 := ScalingConfig{StreamsPerConn: 5, MaxConns: 3}
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg1, insecureOpt)

	// Second call with different config — should use stored config.
	cfg2 := ScalingConfig{StreamsPerConn: 1, MaxConns: 1}
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg2, insecureOpt)

	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	storedSPC := group.cfg.StreamsPerConn
	storedMax := group.cfg.MaxConns
	p.mu.Unlock()

	// Config from first call should be used.
	if storedSPC != 5 {
		t.Fatalf("expected stored StreamsPerConn 5, got %d", storedSPC)
	}
	if storedMax != 3 {
		t.Fatalf("expected stored MaxConns 3, got %d", storedMax)
	}
}

func TestPool_ConnectScaled_DifferentAddresses(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	c2, _ := p.ConnectScaled("passthrough:///localhost:1", cfg, insecureOpt)

	if c1 == c2 {
		t.Fatal("expected different connections for different addresses")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}
}

func TestPool_ConnectScaled_ConcurrentAccess(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 5, MaxConns: 10}
	var wg sync.WaitGroup
	conns := make([]*grpc.ClientConn, 50)
	errs := make([]error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
			conns[idx] = c
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d got unexpected error: %v", i, err)
		}
	}

	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	numConns := len(group.entries)
	var totalStreams int64
	for _, e := range group.entries {
		totalStreams += e.activeStreams
	}
	p.mu.Unlock()

	if totalStreams != 50 {
		t.Fatalf("expected 50 total active streams, got %d", totalStreams)
	}
	if numConns > 10 {
		t.Fatalf("expected at most 10 connections, got %d", numConns)
	}
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

func TestPool_Release_Decrements(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	p.mu.Lock()
	before := p.scaled["passthrough:///localhost:0"].entries[0].activeStreams
	p.mu.Unlock()

	p.Release("passthrough:///localhost:0", conn)

	p.mu.Lock()
	after := p.scaled["passthrough:///localhost:0"].entries[0].activeStreams
	p.mu.Unlock()

	if before != 1 {
		t.Fatalf("expected 1 active stream before release, got %d", before)
	}
	if after != 0 {
		t.Fatalf("expected 0 active streams after release, got %d", after)
	}
}

func TestPool_Release_UnknownAddr(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	// Should not panic.
	p.Release("passthrough:///unknown:0", nil)
}

func TestPool_Release_UnknownConn(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Release with a connection that is not in the pool — should be a no-op.
	otherConn, _ := grpc.NewClient("passthrough:///other:0", insecureOpt)
	defer func() { _ = otherConn.Close() }()

	p.Release("passthrough:///localhost:0", otherConn)

	p.mu.Lock()
	streams := p.scaled["passthrough:///localhost:0"].entries[0].activeStreams
	p.mu.Unlock()

	if streams != 1 {
		t.Fatalf("expected 1 active stream (unchanged), got %d", streams)
	}
}

func TestPool_Release_DoesNotGoBelowZero(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Release once (1 → 0).
	p.Release("passthrough:///localhost:0", conn)
	// Release again (should stay at 0, not go to -1).
	p.Release("passthrough:///localhost:0", conn)

	p.mu.Lock()
	streams := p.scaled["passthrough:///localhost:0"].entries[0].activeStreams
	p.mu.Unlock()

	if streams != 0 {
		t.Fatalf("expected 0 active streams, got %d", streams)
	}
}

func TestPool_Release_Concurrent(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 200, MaxConns: 1}

	conns := make([]*grpc.ClientConn, 100)
	for i := range conns {
		c, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		conns[i] = c
	}

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(conn *grpc.ClientConn) {
			defer wg.Done()
			p.Release("passthrough:///localhost:0", conn)
		}(c)
	}
	wg.Wait()

	p.mu.Lock()
	streams := p.scaled["passthrough:///localhost:0"].entries[0].activeStreams
	p.mu.Unlock()

	if streams != 0 {
		t.Fatalf("expected 0 active streams after releasing all, got %d", streams)
	}
}

// ---------------------------------------------------------------------------
// Reconnect (scaled)
// ---------------------------------------------------------------------------

func TestPool_Reconnect_Scaled(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 3}
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	c2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	if c1 == c2 {
		t.Fatal("expected different connections")
	}

	conn, err := p.Reconnect("passthrough:///localhost:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Returned connection should be different from originals (reconnected).
	if conn == c1 || conn == c2 {
		t.Fatal("expected new connection after reconnect")
	}

	// Active streams should be reset.
	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	for _, e := range group.entries {
		if e.activeStreams != 0 {
			t.Fatalf("expected 0 active streams after reconnect, got %d", e.activeStreams)
		}
	}
	p.mu.Unlock()
}

func TestPool_Reconnect_ScaledDialError(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 2}
	_, _ = p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Sabotage opts.
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	_, err := p.Reconnect("passthrough:///localhost:0")
	if err == nil {
		t.Fatal("expected error when all scaled connections fail to redial")
	}

	// Group should be removed.
	p.mu.Lock()
	_, exists := p.scaled["passthrough:///localhost:0"]
	p.mu.Unlock()

	if exists {
		t.Fatal("expected scaled group to be removed after failed reconnect")
	}
}

func TestPool_Reconnect_BothRegularAndScaled(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	// Create regular connection.
	regularConn, _ := p.Connect("passthrough:///localhost:0", insecureOpt)

	// Create scaled connections for the same address.
	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 2}
	scaledConn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	conn, err := p.Reconnect("passthrough:///localhost:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return the reconnected regular connection.
	if conn == regularConn {
		t.Fatal("expected new regular connection")
	}

	// Scaled connection should also be reconnected.
	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	newScaledConn := group.entries[0].conn
	p.mu.Unlock()

	if newScaledConn == scaledConn {
		t.Fatal("expected scaled connection to be reconnected too")
	}
}

// ---------------------------------------------------------------------------
// Health Watch (scaled)
// ---------------------------------------------------------------------------

func TestPool_HealthWatch_ReconnectsScaledConn(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Simulate dead connection.
	_ = conn.Close()

	p.checkConnections()

	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	newConn := group.entries[0].conn
	streams := group.entries[0].activeStreams
	p.mu.Unlock()

	if newConn == conn {
		t.Fatal("expected new connection after health check")
	}
	if streams != 0 {
		t.Fatalf("expected 0 active streams after reconnect, got %d", streams)
	}
}

func TestPool_HealthWatch_RemovesScaledOnDialError(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	_ = conn.Close()
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	p.checkConnections()

	p.mu.Lock()
	_, exists := p.scaled["passthrough:///localhost:0"]
	p.mu.Unlock()

	if exists {
		t.Fatal("expected scaled group to be removed after failed reconnect")
	}
}

func TestPool_HealthWatch_KeepsHealthyScaledConns(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 3}
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	c2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Kill only the second connection.
	_ = c2.Close()

	p.checkConnections()

	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	numEntries := len(group.entries)
	firstConn := group.entries[0].conn
	p.mu.Unlock()

	// Both entries should still exist (dead one was reconnected).
	if numEntries != 2 {
		t.Fatalf("expected 2 entries, got %d", numEntries)
	}
	// The healthy connection should be unchanged.
	if firstConn != c1 {
		t.Fatal("expected healthy connection to remain unchanged")
	}
}

func TestPool_HealthWatch_PartialScaledDialFailure(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 3}
	c1, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	c2, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Kill c2 and sabotage opts so reconnect fails.
	_ = c2.Close()
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()

	p.checkConnections()

	p.mu.Lock()
	group := p.scaled["passthrough:///localhost:0"]
	numEntries := len(group.entries)
	survivingConn := group.entries[0].conn
	p.mu.Unlock()

	// Only the healthy connection should survive.
	if numEntries != 1 {
		t.Fatalf("expected 1 surviving entry, got %d", numEntries)
	}
	if survivingConn != c1 {
		t.Fatal("expected healthy connection to survive")
	}
}

func TestPool_HealthWatch_ScaledWithTicker(t *testing.T) {
	p := NewPool()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	_, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p.EnableHealthWatch(10 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// Healthy connection should still be alive.
	if p.Len() != 1 {
		t.Fatalf("expected pool size 1, got %d", p.Len())
	}

	_ = p.Close()
}

// ---------------------------------------------------------------------------
// Len
// ---------------------------------------------------------------------------

func TestPool_Len_IncludesScaled(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	_, _ = p.Connect("passthrough:///localhost:0", insecureOpt)

	cfg := ScalingConfig{StreamsPerConn: 1, MaxConns: 5}
	_, _ = p.ConnectScaled("passthrough:///localhost:1", cfg, insecureOpt)
	_, _ = p.ConnectScaled("passthrough:///localhost:1", cfg, insecureOpt)

	// 1 regular + 2 scaled = 3
	if p.Len() != 3 {
		t.Fatalf("expected pool size 3, got %d", p.Len())
	}
}

// ---------------------------------------------------------------------------
// Integration: Connect and ConnectScaled coexistence
// ---------------------------------------------------------------------------

func TestPool_ConnectAndScaledCoexist(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	// Regular connection for addr A.
	regular, _ := p.Connect("passthrough:///localhost:0", insecureOpt)

	// Scaled connection for addr B.
	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	scaled, _ := p.ConnectScaled("passthrough:///localhost:1", cfg, insecureOpt)

	if regular == nil || scaled == nil {
		t.Fatal("expected non-nil connections")
	}
	if regular == scaled {
		t.Fatal("expected different connections for different addresses")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}
}

func TestPool_ConnectAndScaled_SameAddr(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	// Both regular and scaled on the same address.
	regular, _ := p.Connect("passthrough:///localhost:0", insecureOpt)
	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	scaled, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// They should be different connections (separate maps).
	if regular == scaled {
		t.Fatal("expected different connections from Connect and ConnectScaled")
	}
	if p.Len() != 2 {
		t.Fatalf("expected pool size 2, got %d", p.Len())
	}
}

// ---------------------------------------------------------------------------
// Edge case: Release after health watch removes connection
// ---------------------------------------------------------------------------

func TestPool_Release_AfterHealthWatchRemovesConn(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Simulate health watch removing the connection.
	_ = conn.Close()
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()
	p.checkConnections()

	// Release should be a no-op (group removed).
	p.Release("passthrough:///localhost:0", conn)
}

// ---------------------------------------------------------------------------
// Edge case: ConnectScaled after all entries removed by health watch
// ---------------------------------------------------------------------------

func TestPool_ConnectScaled_AfterHealthWatchRemovesAll(t *testing.T) {
	p := NewPool()
	defer func() { _ = p.Close() }()

	cfg := ScalingConfig{StreamsPerConn: 100, MaxConns: 5}
	conn, _ := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)

	// Simulate health watch removing all entries.
	_ = conn.Close()
	p.mu.Lock()
	p.scaled["passthrough:///localhost:0"].opts = nil
	p.mu.Unlock()
	p.checkConnections()

	// Group should be removed, so next ConnectScaled creates a fresh group.
	conn2, err := p.ConnectScaled("passthrough:///localhost:0", cfg, insecureOpt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn2 == conn {
		t.Fatal("expected new connection")
	}
}
