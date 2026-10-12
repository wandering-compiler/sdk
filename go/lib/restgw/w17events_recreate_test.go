package restgw_test

import (
	"context"
	"net"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// INVARIANT: the SSE hub resumes delivery after the broker loses its
// consumer — here the broker is recreated without its JetStream state, as an
// ordinary `docker compose up -d` does to a non-persistent NATS. Without the
// bus's consumer recovery every connected client stops receiving events for
// the rest of the gateway's life, and nothing reports it to the client.
func TestEventbusEventSource_ResumesAfterTheBrokerRecreatesItsConsumer(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready")
	}
	dsn := srv.ClientURL()
	port := srv.Addr().(*net.TCPAddr).Port

	r := startHubReplica(t, dsn, "tasks", "task.created")
	producer, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "APP_STORAGE-subscribers"})
	if err != nil {
		t.Fatalf("NewNatsBus(producer): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = producer.Close(ctx)
	})
	expectFrame := func(budget time.Duration, what string) {
		t.Helper()
		select {
		case ev := <-r.frames:
			if ev.Topic != "task.created" {
				t.Fatalf("%s: got topic %q", what, ev.Topic)
			}
		case <-time.After(budget):
			t.Fatalf("%s: the hub's client never received the event", what)
		}
	}
	if err := producer.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String("before")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	expectFrame(15*time.Second, "before the broker was recreated")

	srv.Shutdown()
	srv.WaitForShutdown()
	opts2 := natsserver.DefaultTestOptions
	opts2.Port = port
	opts2.JetStream = true
	opts2.StoreDir = t.TempDir() // empty: the hub's consumer is gone
	srv2 := natsserver.RunServer(&opts2)
	t.Cleanup(srv2.Shutdown)
	if !srv2.ReadyForConnections(5 * time.Second) {
		t.Fatal("recreated nats-server not ready")
	}

	// Emit until one reaches the client: the producer and the hub both have
	// to reconnect and re-create what the broker lost.
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = producer.Dispatch(ctx, "tasks", "task.created", wrapperspb.String("after"))
		cancel()
		select {
		case ev := <-r.frames:
			if ev.Topic != "task.created" {
				t.Fatalf("after the broker was recreated: got topic %q", ev.Topic)
			}
			return
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub never resumed delivery after the broker recreated its consumer")
		}
	}
}
