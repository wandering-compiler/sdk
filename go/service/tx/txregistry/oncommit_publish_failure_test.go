package txregistry_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

type emitFailureObserver struct {
	eventbus.NopObserver
	mu       sync.Mutex
	failures []error
	topics   []string
}

func (o *emitFailureObserver) OnEmitFailure(_, topic string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, err)
	o.topics = append(o.topics, topic)
}

// The emit a committed transaction parked runs after the commit, where its
// error has no caller to go back to (the generated helper discards it). A
// publish that fails there must still leave a trace: the bus's Observer
// hears OnEmitFailure with the topic, and the commit itself stands.
func TestOnCommit_AFailedPublishIsReportedThroughTheObserver(t *testing.T) {
	reg, id := openReg(t)
	obs := &emitFailureObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})
	sub, err := bus.Subscriber(context.Background(), "tasks")
	if err != nil {
		t.Fatal(err)
	}
	// A drained bus refuses every publish: the broker-outage stand-in.
	if err := sub.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !reg.OnCommit(id, func() {
		_ = bus.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String("t-1"))
	}) {
		t.Fatal("OnCommit should resolve a live tx id")
	}
	if err := reg.Commit(context.Background(), id); err != nil {
		t.Fatalf("Commit: %v; a failed after-commit publish must not fail the commit", err)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.failures) != 1 || obs.topics[0] != "task.created" || !errors.Is(obs.failures[0], eventbus.ErrBusDraining) {
		t.Fatalf("OnEmitFailure = %v %v, want one ErrBusDraining for task.created", obs.topics, obs.failures)
	}
}
