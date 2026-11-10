package eventlog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// A failed batch behind more than one page of pending entries that are not
// due (another process's, still inside AckWait) must still be offered again:
// the claim loop pages on past them instead of reading only the first page.
func TestRedisConsumers_AFailedBatchBehindAFullPageIsOfferedAgain(t *testing.T) {
	_, c := redisFixture(t)
	recordTwoRedis(t, c)
	ctx := context.Background()
	first, err := c.XRange(ctx, RedisStreamKey, "-", "+").Result()
	if err != nil || len(first) != 1 {
		t.Fatalf("XRANGE: %v (%d entries)", err, len(first))
	}
	for range 64 {
		if err := c.XAdd(ctx, &goredis.XAddArgs{Stream: RedisStreamKey, Values: first[0].Values}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	const group = "test-audit-a"
	if err := ensureRedisGroup(ctx, c, group); err != nil {
		t.Fatal(err)
	}
	// 64 entries pending on another process, then one failed here.
	if err := c.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: group, Consumer: "other", Streams: []string{RedisStreamKey, ">"}, Count: 64}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := c.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: group, Consumer: "me", Streams: []string{RedisStreamKey, ">"}, Count: 1}).Err(); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("a", funcConsumer{audit: func(context.Context, *ConsumedBatch) error {
		calls.Add(1)
		return nil
	}})
	stop := runRedisConsumers(t, cs, c, ConsumerConfig{
		ConsumerName: "me", AckWait: time.Hour, Backoff: func(int) time.Duration { return 10 * time.Millisecond },
	})
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("the failed batch behind a full page of pending entries was never offered again")
	}
}
