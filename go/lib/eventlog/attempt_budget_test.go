package eventlog

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A Consume that times out is offered again after its backoff, not at once.
//
// The attempt used to be bounded by exactly the consumer's AckWait — the
// moment the broker redelivers on its own. The timed-out attempt's
// NakWithDelay then raced the broker's redelivery and lost: the batch came
// back immediately, its backoff skipped. The attempt now ends a tenth
// before AckWait, so the Nak with its delay is what the broker acts on.
func TestConsumers_TimedOutAttemptKeepsItsBackoff(t *testing.T) {
	_, js := embeddedJetStream(t)
	recordTwo(t, js)
	var (
		mu    sync.Mutex
		calls []time.Time
	)
	c := funcConsumer{audit: func(ctx context.Context, _ *ConsumedBatch) error {
		mu.Lock()
		calls = append(calls, time.Now())
		first := len(calls) == 1
		mu.Unlock()
		if first {
			<-ctx.Done()
			// A call unwinds after its ctx ends (a gRPC client returns once
			// the cancellation reached it), so the outcome is settled a
			// little after the deadline, never exactly on it.
			time.Sleep(100 * time.Millisecond)
			return ctx.Err()
		}
		return nil
	}}
	cs := &Consumers{}
	if err := cs.Audit("hung", c); err != nil {
		t.Fatal(err)
	}
	const ackWait, backoff = 2 * time.Second, 2 * time.Second
	stop := runConsumers(t, cs, js, ConsumerConfig{AckWait: ackWait, MaxDeliver: 10, Backoff: func(int) time.Duration { return backoff }})
	defer stop()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("the batch was never offered again (%d calls)", len(calls))
	}
	if gap := calls[1].Sub(calls[0]); gap < backoff+ackWait/2 {
		t.Errorf("second attempt %v after the first; want at least the attempt (~%v) plus the backoff %v — the timed-out attempt's backoff was skipped", gap, ackWait, backoff)
	}
}
