package eventbus_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// NewNatsBus connects up front: against a broker that is not there it must
// fail at construction, not hand back a bus whose first emit finds out.
func TestNewNatsBus_UnreachableBrokerFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens on addr now

	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: "nats://" + addr, DurablePrefix: "unreachable"})
	if err == nil {
		_ = bus.Close(context.Background())
		t.Fatal("NewNatsBus against an unreachable broker returned a bus and no error")
	}
}

// emitFailures counts OnEmitFailure; the rest of the observer is silent.
type emitFailures struct {
	eventbus.NopObserver
	n atomic.Int32
}

func (o *emitFailures) OnEmitFailure(string, string, error) { o.n.Add(1) }

// Once the client stops reconnecting the bus can deliver nothing for the rest
// of the process. That must be said ("closed for good" plus an error report),
// and every later Dispatch must fail at once through OnEmitFailure instead of
// spending PublishTimeout per event finding it out.
//
// nats.go gives up after two consecutive authorization failures on reconnect,
// so the broker comes back on the same port demanding credentials the bus
// does not have.
func TestNatsBus_ClosedForGoodIsSaidAndFailsDispatchAtOnce(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready")
	}
	port := srv.Addr().(*net.TCPAddr).Port
	text := captureLogs(t)

	obs := &emitFailures{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:            srv.ClientURL(),
		DurablePrefix:  "closedforgood",
		Observer:       obs,
		PublishTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	srv.Shutdown()
	locked := natsserver.DefaultTestOptions
	locked.Port = port
	locked.Username, locked.Password = "someone", "else"
	srv2 := natsserver.RunServer(&locked)
	t.Cleanup(srv2.Shutdown)

	waitFor(t, 20*time.Second, "the bus never said it was closed for good", func() bool {
		return strings.Contains(text(), "CLOSED for good")
	})
	waitFor(t, 2*time.Second, "the closed connection was not reported as an error", func() bool {
		return strings.Contains(text(), "error: eventbus nats: connection closed permanently")
	})

	for i := 0; i < 3; i++ {
		start := time.Now()
		err := bus.Dispatch(context.Background(), "ch", "a.b", wrapperspb.String("x"))
		if err == nil {
			t.Fatal("Dispatch on a connection closed for good succeeded")
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("Dispatch %d took %v to fail; want at once, not PublishTimeout", i, d)
		}
	}
	if got := obs.n.Load(); got != 3 {
		t.Errorf("OnEmitFailure fired %d times, want 3", got)
	}
}

// stallProxy forwards TCP to target. stall() makes it keep every connection
// open and answer nothing — a broker that hangs rather than refusing; cut()
// drops the connections it holds and keeps accepting new ones — a link that
// is lost and comes back.
type stallProxy struct {
	addr    string
	stalled atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func newStallProxy(t *testing.T, target string) *stallProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{addr: ln.Addr().String()}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut()
		wg.Wait()
	})
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 && !p.stalled.Load() {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					_ = src.Close()
					return
				}
			}
			if err != nil {
				_ = dst.Close()
				return
			}
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			wg.Add(2)
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return p
}

func (p *stallProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *stallProxy) stall() { p.stalled.Store(true) }

// A Redis broker that stops answering must cost one Dispatch PublishTimeout,
// not the client's own read timeout: the emit runs on the committing
// goroutine.
func TestRedisBus_PublishTimeoutBoundsAHangingBroker(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newStallProxy(t, mr.Addr())
	obs := &emitFailures{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		// The client's own timeouts are far longer than PublishTimeout, so
		// only PublishTimeout can end the call in time.
		DSN:            "redis://" + proxy.addr + "?read_timeout=30s&write_timeout=30s&max_retries=-1",
		GroupPrefix:    "hang",
		Observer:       obs,
		PublishTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 2*time.Second)) })
	if err := bus.Dispatch(context.Background(), "ch", "a.b", wrapperspb.String("before")); err != nil {
		t.Fatalf("Dispatch before the stall: %v", err)
	}

	proxy.stall()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- bus.Dispatch(context.Background(), "ch", "a.b", wrapperspb.String("during")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dispatch against a hanging broker succeeded")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("Dispatch took %v; want about PublishTimeout (300ms)", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch against a hanging broker did not return in 5s; PublishTimeout does not bound it")
	}
	if obs.n.Load() < 1 {
		t.Error("the timed-out Dispatch did not reach OnEmitFailure")
	}
}

// A read batch larger than 100 entries is delivered whole: every entry of a
// 300-entry backlog read 250 at a time reaches the handler.
func TestRedisBus_ReadBatchAbove100LosesNothing(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         dsn,
		GroupPrefix: "bigbatch",
		ReadBatch:   250,
		Observer:    eventbus.NopObserver{},
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	const total = 300
	for i := 0; i < total; i++ {
		if err := bus.Dispatch(context.Background(), "big", "a.b", wrapperspb.String(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	sub, err := bus.Subscriber(context.Background(), "big")
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Subscribe(context.Background(), "**", func(_ context.Context, _ string, raw []byte) error {
		mu.Lock()
		seen[string(raw)] = true
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "every entry of a 250-entry read batch delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == total
	})
}

// The dead-letter stream is trimmed on append like a channel stream: by the
// configured count, or by the default age when nothing is configured.
func TestRedisBus_DLQRetention_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	raw := rawRedisClient(t, dsn)
	var drops atomic.Int32
	obs := &dropCounter{n: &drops}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       "dlqret",
		DefaultMaxDeliver: 1,
		DefaultAckWait:    50 * time.Millisecond,
		DLQRetention:      eventbus.StreamRetention{MaxAge: -1, MaxMsgs: 2},
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(context.Background(), "poison")
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		return errors.New("always fails")
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := bus.Dispatch(context.Background(), "poison", "a.b", wrapperspb.String(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 10*time.Second, "all four entries dead-lettered", func() bool { return drops.Load() >= 4 })
	if n := raw.XLen(context.Background(), "poison.dlq").Val(); n != 2 {
		t.Errorf("XLEN poison.dlq = %d, want 2 (trimmed to DLQRetention.MaxMsgs)", n)
	}
}

type dropCounter struct {
	eventbus.NopObserver
	n *atomic.Int32
}

func (o *dropCounter) OnDrop(string, string, string) { o.n.Add(1) }
