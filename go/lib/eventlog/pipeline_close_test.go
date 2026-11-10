package eventlog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A Close that has begun must not stall new submitters behind a submitter
// still waiting for room: a Submit after Close started fails with ErrClosed
// at once instead of queuing behind the waiter for its BlockTimeout.
func TestPipeline_SubmitDuringACloseThatWaitsFailsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.gate = make(chan struct{})
	cfg := h.config()
	cfg.QueueBytes, cfg.BlockTimeout = 16, 2*time.Second
	p := start(t, cfg)
	_ = p.Submit(context.Background(), &rec{n: 0}) // fills the queue; the sink holds it

	waiting := make(chan error, 1)
	go func() { waiting <- p.Submit(context.Background(), &rec{n: 1}) }()
	time.Sleep(100 * time.Millisecond) // the second submitter waits for room

	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closed <- p.Close(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // Close has begun

	t0 := time.Now()
	err := p.Submit(context.Background(), &rec{n: 2})
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Errorf("a Submit during Close waited %v behind the submitter waiting for room", d)
	}
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Submit during Close = %v, want ErrClosed", err)
	}
	close(h.gate)
	<-waiting
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
}
