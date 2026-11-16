package eventbus_test

// A delivery attempt is bounded by the channel's AckWait: the handler's ctx
// carries a deadline inside it, so a handler that honours its ctx (a gRPC
// call does) stops before the broker hands the same message to another
// replica. Without the deadline a run past AckWait goes on while the
// redelivery runs it again — two concurrent executions of one event.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// overlapProbe is a handler that blocks until its ctx ends (or a hard cap)
// on its first run and succeeds at once afterwards, recording how many runs
// were in flight at the same time and whether the first run had a deadline.
type overlapProbe struct {
	mu          sync.Mutex
	running     int
	maxRunning  int
	runs        atomic.Int32
	firstHadDL  atomic.Bool
	firstBudget atomic.Int64
}

func (p *overlapProbe) handle(ctx context.Context, _ string, _ []byte) error {
	n := p.runs.Add(1)
	p.mu.Lock()
	p.running++
	if p.running > p.maxRunning {
		p.maxRunning = p.running
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}()
	if n == 1 {
		if dl, ok := ctx.Deadline(); ok {
			p.firstHadDL.Store(true)
			p.firstBudget.Store(int64(time.Until(dl)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	return nil
}

func (p *overlapProbe) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxRunning
}

func assertBoundedAttempt(t *testing.T, p *overlapProbe, ackWait time.Duration) {
	t.Helper()
	waitFor(t, 10*time.Second, "a second delivery runs", func() bool { return p.runs.Load() >= 2 })
	time.Sleep(200 * time.Millisecond)
	if !p.firstHadDL.Load() {
		t.Errorf("the handler's ctx carries no deadline: an attempt is not bounded by AckWait (%v)", ackWait)
	} else if b := time.Duration(p.firstBudget.Load()); b <= 0 || b > ackWait {
		t.Errorf("attempt budget %v, want within (0, AckWait=%v]", b, ackWait)
	}
	if got := p.peak(); got != 1 {
		t.Errorf("%d runs of one event in flight at once; an attempt past AckWait must stop before the redelivery runs", got)
	}
}

func TestNatsBus_HandlerAttemptBoundedByAckWait_Embedded(t *testing.T) {
	dsn := startEmbeddedNATS(t)
	const ackWait = time.Second
	probe := &overlapProbe{}
	var buses []*eventbus.NatsBus
	for i := 0; i < 2; i++ {
		bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
			DSN:               dsn,
			DurablePrefix:     "deadline-subscribers",
			DefaultMaxDeliver: 5,
			DefaultAckWait:    ackWait,
		})
		if err != nil {
			t.Fatalf("NewNatsBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
		sub, err := bus.Subscriber(context.Background(), "default")
		if err != nil {
			t.Fatalf("Subscriber: %v", err)
		}
		if err := sub.Subscribe(context.Background(), "**", probe.handle); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		buses = append(buses, bus)
	}
	if err := buses[0].Dispatch(context.Background(), "default", "t", wrapperspb.String("slow")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	assertBoundedAttempt(t, probe, ackWait)
}

func TestRedisBus_HandlerAttemptBoundedByAckWait_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	const ackWait = time.Second
	probe := &overlapProbe{}
	var buses []*eventbus.RedisBus
	for i := 0; i < 2; i++ {
		bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
			DSN:               dsn,
			GroupPrefix:       "deadline-subscribers",
			ConsumerName:      []string{"r0", "r1"}[i],
			DefaultMaxDeliver: 5,
			DefaultAckWait:    ackWait,
		})
		if err != nil {
			t.Fatalf("NewRedisBus: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
		sub, err := bus.Subscriber(context.Background(), "default")
		if err != nil {
			t.Fatalf("Subscriber: %v", err)
		}
		if err := sub.Subscribe(context.Background(), "**", probe.handle); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		buses = append(buses, bus)
	}
	if err := buses[0].Dispatch(context.Background(), "default", "t", wrapperspb.String("slow")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	assertBoundedAttempt(t, probe, ackWait)
}
