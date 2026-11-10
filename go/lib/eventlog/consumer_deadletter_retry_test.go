package eventlog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A dead-letter publish that fails on the MaxDeliver-th delivery must not
// strand the batch: the broker consumer has no cap of its own (the consumer
// counts attempts), so the batch is offered again and dead-lettered once the
// dead letter stream takes it, without calling the consumer again.
func TestConsumers_AFailedDeadLetterAtTheCapIsRetried(t *testing.T) {
	_, js := embeddedJetStream(t)
	recordTwo(t, js)
	var calls atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("broken", funcConsumer{audit: func(ctx context.Context, _ *ConsumedBatch) error {
		calls.Add(1)
		// The DLQ stream goes away before the consumer dead-letters: a
		// broker that lost it, storage full, a transient refusal.
		_ = js.DeleteStream(context.Background(), DeadLetterStream)
		return errors.New("broken")
	}})
	stop := runConsumers(t, cs, js, ConsumerConfig{MaxDeliver: 1, Backoff: func(int) time.Duration { return 20 * time.Millisecond }})
	defer stop()
	waitUntil(t, func() bool { return calls.Load() >= 1 })
	time.Sleep(3 * time.Second)
	// The stream is back; a redelivery would dead-letter now.
	if err := EnsureStream(context.Background(), js, DeadLetterStream, []string{DeadLetterPrefix + ".>"}, StreamLimits{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	ci, err := js.Consumer(context.Background(), StreamName, "test-audit-broken")
	if err != nil {
		cons, lerr := js.Stream(context.Background(), StreamName)
		if lerr == nil {
			names := cons.ConsumerNames(context.Background())
			for n := range names.Name() {
				t.Logf("consumer: %s", n)
			}
		}
		t.Fatalf("consumer lookup: %v", err)
	}
	info, _ := ci.Info(context.Background())
	dl, _ := js.Stream(context.Background(), DeadLetterStream)
	dli, _ := dl.Info(context.Background())
	t.Logf("calls=%d consumer: delivered=%d ack_pending=%d pending=%d redelivered=%d; dlq msgs=%d",
		calls.Load(), info.Delivered.Consumer, info.NumAckPending, info.NumPending, info.NumRedelivered, dli.State.Msgs)
	if dli.State.Msgs != 1 {
		t.Fatalf("the batch is not dead-lettered once the stream is back (calls=%d, ack_pending=%d, dlq=%d)", calls.Load(), info.NumAckPending, dli.State.Msgs)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the consumer was called %d times; past its cap the batch goes to the dead letters without another call", n)
	}
}
