package eventbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// retryBrokerOp is the bounded retry that rides out a transient broker
// failure during DLQ routing (Q61-bus-1). These cases pin its contract:
// succeed-on-retry, exhaust-and-return-last-error, and ctx-cancel-abort.
func TestRetryBrokerOp_SucceedsAfterTransientFailure(t *testing.T) {
	calls := 0
	transient := errors.New("broker hiccup")
	err := retryBrokerOp(context.Background(), 3, time.Microsecond, func() error {
		calls++
		if calls < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want success once the op recovers, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts (2 failures then success), got %d", calls)
	}
}

func TestRetryBrokerOp_ExhaustsAndReturnsLastError(t *testing.T) {
	calls := 0
	last := errors.New("still down")
	err := retryBrokerOp(context.Background(), 3, time.Microsecond, func() error {
		calls++
		return last
	})
	if !errors.Is(err, last) {
		t.Fatalf("want the last attempt's error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want exactly 3 attempts before giving up, got %d", calls)
	}
}

func TestRetryBrokerOp_CancelledContextAbortsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryBrokerOp(ctx, 5, time.Hour, func() error {
		calls++
		return errors.New("fail")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled once ctx is done, got %v", err)
	}
	// One attempt runs, then the cancelled ctx aborts the backoff wait
	// instead of blocking for the (1h) backoff.
	if calls != 1 {
		t.Fatalf("want the retry to abort after the first failure, got %d calls", calls)
	}
}
