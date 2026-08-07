package eventbus_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A message waiting its turn in a batch must not be reclaimed as abandoned.
//
// XREADGROUP takes up to ReadBatch messages in ONE read — every one of them
// enters the pending list at that moment — and the loop then hands them to
// the handler one at a time. The in-flight guard that keeps the claim loop
// off a message was only set inside processMessage, so a sibling still
// queued behind a slow handler was: pending, idle past AckWait, and invisible
// to that guard. Which is everything the claim loop needs to XCLAIM it and
// run the handler for it — concurrently, in its own goroutine, while the
// consume loop is still going to run the very same message when its turn
// comes.
//
// With MaxDeliver reached it is worse than a double-run: the claim tick
// dead-letters the sibling and XACKs it, and the consume loop then runs it
// successfully anyway. One message, both dead-lettered and delivered, which
// is precisely the incoherence the guard's own comment cites R-bus-1 as
// preventing.
//
// Two concurrent actors, both inside one subscriber: its consumeLoop
// goroutine and its claimLoop goroutine.
func TestRedisBus_BatchSiblingIsNotReclaimedWhileQueued(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         dsn,
		GroupPrefix: "batch-inflight-subscribers",
		// One retry, so a reclaimed sibling goes straight to the DLQ and the
		// incoherence is observable rather than merely duplicated.
		DefaultMaxDeliver: 1,
		// Short enough that a claim tick lands while the first handler is
		// still working — the whole point of the window.
		DefaultAckWait: 150 * time.Millisecond,
		ReadBatch:      10,
		Observer:       obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}

	var first, second atomic.Int32
	if err := sub.Subscribe(context.Background(), "**",
		func(_ context.Context, topic string, _ []byte) error {
			switch topic {
			case "slow":
				first.Add(1)
				// Outlast AckWait so the claim loop ticks while the sibling
				// is queued behind this handler.
				time.Sleep(600 * time.Millisecond)
			case "sibling":
				second.Add(1)
			}
			return nil
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Both dispatched before the reader wakes, so they arrive in ONE batch.
	if err := bus.Dispatch(context.Background(), "default", "slow", wrapperspb.String("1")); err != nil {
		t.Fatalf("dispatch slow: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "default", "sibling", wrapperspb.String("2")); err != nil {
		t.Fatalf("dispatch sibling: %v", err)
	}

	waitFor(t, 10*time.Second, "both messages handled", func() bool {
		return first.Load() >= 1 && second.Load() >= 1
	})
	// Give any duplicate run or DLQ routing time to show up.
	time.Sleep(700 * time.Millisecond)

	if got := second.Load(); got != 1 {
		t.Errorf("the queued sibling was handled %d times, want exactly 1 — the claim loop "+
			"reclaimed a message that was never abandoned, only waiting its turn", got)
	}

	raw := rawRedisClient(t, dsn)
	xlen, err := raw.XLen(context.Background(), "default.dlq").Result()
	if err != nil {
		t.Fatalf("XLEN default.dlq: %v", err)
	}
	if xlen != 0 {
		t.Errorf("DLQ holds %d entry(ies): a message that the handler went on to process "+
			"successfully was also dead-lettered — one message reported as both dropped "+
			"and delivered", xlen)
	}
	if _, _, _, _, drops := obs.snapshots(); drops != 0 {
		t.Errorf("OnDrop fired %d time(s) for a batch nothing failed in", drops)
	}
}
