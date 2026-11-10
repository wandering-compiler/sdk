package eventlog

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingAudit is an audit consumer that records how often it saw each
// batch and takes `each` per batch.
type countingAudit struct {
	mu   sync.Mutex
	seen map[string]int
	each time.Duration
}

func (c *countingAudit) consumer() funcConsumer {
	return funcConsumer{audit: func(_ context.Context, b *ConsumedBatch) error {
		c.mu.Lock()
		if c.seen == nil {
			c.seen = map[string]int{}
		}
		c.seen[b.ID.String()]++
		c.mu.Unlock()
		time.Sleep(c.each)
		return nil
	}}
}

func (c *countingAudit) check(t *testing.T, batches int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) != batches {
		t.Errorf("%d distinct batches consumed, want %d", len(c.seen), batches)
	}
	for id, n := range c.seen {
		if n != 1 {
			t.Errorf("batch %s consumed %d times although no Consume failed — it waited past AckWait behind the batches before it", id, n)
		}
	}
}

// A batch the consumer holds but has not begun to consume must not age past
// AckWait.
//
// The loop used to Fetch 16 messages and consume them one after another,
// while JetStream ran each one's AckWait from the moment it was fetched: the
// batches queued behind a slow Consume were redelivered although none had
// failed, consumed twice, and their delivery count — which the loop reads
// as the attempt number — grew without a failure.
func TestConsumers_QueuedBatchesAreNotRedelivered(t *testing.T) {
	_, js := embeddedJetStream(t)
	const n = 6
	for i := 0; i < n; i++ {
		recordTwo(t, js)
	}
	c := &countingAudit{each: 250 * time.Millisecond}
	cs := &Consumers{}
	if err := cs.Audit("slow", c.consumer()); err != nil {
		t.Fatal(err)
	}
	stop := runConsumers(t, cs, js, ConsumerConfig{AckWait: 400 * time.Millisecond, MaxDeliver: 10})
	time.Sleep(4 * time.Second)
	stop()
	c.check(t, n)
}

// The Redis counterpart: another process's claim loop takes over an entry
// pending past AckWait, so entries read together and queued behind a slow
// Consume were consumed by both processes.
func TestConsumers_QueuedBatchesAreNotRedelivered_Redis(t *testing.T) {
	_, rc := redisFixture(t)
	const n = 6
	for i := 0; i < n; i++ {
		recordTwoRedis(t, rc)
	}
	c := &countingAudit{each: 250 * time.Millisecond}
	csA := &Consumers{}
	if err := csA.Audit("slow", c.consumer()); err != nil {
		t.Fatal(err)
	}
	csB := &Consumers{}
	if err := csB.Audit("slow", c.consumer()); err != nil {
		t.Fatal(err)
	}
	cfg := ConsumerConfig{AckWait: 400 * time.Millisecond, MaxDeliver: 10, Backoff: func(int) time.Duration { return 20 * time.Millisecond }}
	cfgA, cfgB := cfg, cfg
	cfgA.ConsumerName, cfgB.ConsumerName = "process-a", "process-b"
	stopA := runRedisConsumers(t, csA, rc, cfgA)
	time.Sleep(100 * time.Millisecond)
	stopB := runRedisConsumers(t, csB, rc, cfgB)
	time.Sleep(4 * time.Second)
	stopA()
	stopB()
	c.check(t, n)
}
