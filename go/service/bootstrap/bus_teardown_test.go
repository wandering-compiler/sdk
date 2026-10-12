package bootstrap_test

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// The event bus is a resource: it is closed only after every transport has
// drained, so an event a handler emits while its request finishes during
// shutdown is still published and stored, not refused by a closed bus.
func TestRunGraceful_EventBusClosesAfterTheLastDrainingEmit(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: srv.ClientURL(), DurablePrefix: "teardown"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}

	emitErr := make(chan error, 1)
	transport := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // a request still finishing
		emitErr <- bus.Dispatch(context.Background(), "orders", "order.placed", wrapperspb.String("late"))
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.RunGraceful(ctx, testObs, []func(context.Context) error{bus.Close}, transport)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunGraceful = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunGraceful did not return")
	}
	if err := <-emitErr; err != nil {
		t.Fatalf("an emit during the drain failed: %v", err)
	}

	nc, err := natsgo.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	s, err := js.Stream(context.Background(), "orders")
	if err != nil {
		t.Fatalf("the drained emit's stream: %v", err)
	}
	if info, err := s.Info(context.Background()); err != nil || info.State.Msgs != 1 {
		t.Errorf("stream holds %v messages (%v), want the event emitted during the drain", info, err)
	}
	if err := bus.Dispatch(context.Background(), "orders", "order.placed", wrapperspb.String("after")); err == nil {
		t.Error("the bus still accepted an emit after teardown: it was not closed")
	}
}
