package eventbus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A pending entry whose stream row retention removed (it failed, waited for
// its retry, and was trimmed meanwhile) is settled and reported as a drop.
// It used to stay in the group's pending list for good: XCLAIM of an id the
// stream no longer has returns nothing, the claim loop skipped it, the DLQ
// path could not read its payload and left it pending too, and every tick
// listed it again — an event lost without a trace, and a pending list that
// only grows. (Redis 7 drops the row from the pending list inside XCLAIM, so
// there it vanished silently instead: no drop reported either way.)
//
// Needs a real Redis (W17_REDIS_ADDR): miniredis hides a deleted entry from
// XPENDING, which no Redis server does.
func TestRedisBus_TrimmedPendingEntryIsSettled_Live(t *testing.T) {
	addr := os.Getenv("W17_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("W17_REQUIRE_LIVE_REDIS") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_REDIS=1 but W17_REDIS_ADDR is unset")
		}
		t.Skip("W17_REDIS_ADDR unset: needs a real Redis")
	}
	dsn := "redis://" + addr
	channel := fmt.Sprintf("trimmed%d", time.Now().UnixNano())
	obs := &recordingObserver{}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       channel + "-subscribers",
		DefaultMaxDeliver: 3,
		DefaultAckWait:    200 * time.Millisecond,
		Observer:          obs,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, _ := bus.Subscriber(context.Background(), channel)
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		return errors.New("downstream is down")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Dispatch(context.Background(), channel, "t", wrapperspb.String("doomed")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitFor(t, 5*time.Second, "the first delivery fails", func() bool {
		_, _, _, df, _ := obs.snapshots()
		return df >= 1
	})

	raw := rawRedisClient(t, dsn)
	// Retention trims the stream under the pending entry.
	if err := raw.XTrimMaxLen(context.Background(), channel, 0).Err(); err != nil {
		t.Fatalf("XTRIM: %v", err)
	}

	trimmedDrops := func() int {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		n := 0
		for _, d := range obs.drop {
			if d.Reason == eventbus.DropReasonTrimmed {
				n++
			}
		}
		return n
	}
	pending := func() int64 {
		groups, err := raw.XInfoGroups(context.Background(), channel).Result()
		if err != nil || len(groups) == 0 {
			return -1
		}
		return groups[0].Pending
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (trimmedDrops() == 0 || pending() != 0) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := trimmedDrops(); n != 1 {
		t.Errorf("%d %q drops reported for the entry retention removed, want 1", n, eventbus.DropReasonTrimmed)
	}
	if p := pending(); p != 0 {
		t.Errorf("pending list holds %d entries, want 0 (the trimmed entry settled)", p)
	}
}
