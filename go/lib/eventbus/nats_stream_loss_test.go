package eventbus_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A broker that loses a stream (recreated without persistent storage, or
// the stream deleted) must not leave the process unable to emit or consume
// on that channel until it restarts. The emit side re-creates the stream;
// the subscribe side re-creates the consumer and resumes delivery.
func TestNatsBus_RecoversFromStreamLoss_Embedded(t *testing.T) {
	dsn := startEmbeddedNATS(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "loss", Observer: obs})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sub, err := bus.Subscriber(ctx, "lossy")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int32
	if err := sub.Subscribe(ctx, "**", func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Dispatch(ctx, "lossy", "a.b", wrapperspb.String("before")); err != nil {
		t.Fatalf("Dispatch before loss: %v", err)
	}
	waitFor(t, 10*time.Second, "first event delivered", func() bool { return delivered.Load() == 1 })

	nc, err := natsgo.Connect(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteStream(ctx, "lossy"); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}

	for i := 0; i < 3; i++ {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		err := bus.Dispatch(dctx, "lossy", "a.b", wrapperspb.String("after"))
		dcancel()
		if err != nil {
			t.Fatalf("Dispatch #%d after the broker lost the stream: %v", i, err)
		}
	}
	waitFor(t, 30*time.Second, "events emitted after the stream loss delivered to the subscriber", func() bool {
		return delivered.Load() == 4
	})
}

// The same recovery when the whole broker is replaced by one without the
// old JetStream state (a restart without persistent storage): the client
// reconnects to a server that knows neither the stream nor the consumer.
func TestNatsBus_RecoversFromBrokerRecreated_Embedded(t *testing.T) {
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

	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "recreated"})
	if err != nil {
		srv.Shutdown()
		t.Fatalf("NewNatsBus: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sub, err := bus.Subscriber(ctx, "fresh")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var delivered atomic.Int32
	if err := sub.Subscribe(ctx, "**", func(context.Context, string, []byte) error {
		delivered.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Dispatch(ctx, "fresh", "a.b", wrapperspb.String("before")); err != nil {
		t.Fatalf("Dispatch before restart: %v", err)
	}
	waitFor(t, 10*time.Second, "first event delivered", func() bool { return delivered.Load() == 1 })

	srv.Shutdown()
	srv.WaitForShutdown()
	opts2 := natsserver.DefaultTestOptions
	opts2.Port = port
	opts2.JetStream = true
	opts2.StoreDir = t.TempDir() // empty: the old streams and consumers are gone
	srv2 := natsserver.RunServer(&opts2)
	t.Cleanup(srv2.Shutdown)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	if !srv2.ReadyForConnections(5 * time.Second) {
		t.Fatal("recreated nats-server not ready")
	}

	// Emit until the client has reconnected; each emit must then succeed.
	waitFor(t, 20*time.Second, "emit succeeds against the recreated broker", func() bool {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		defer dcancel()
		return bus.Dispatch(dctx, "fresh", "a.b", wrapperspb.String("after")) == nil
	})
	waitFor(t, 60*time.Second, "event emitted after the broker was recreated delivered", func() bool {
		return delivered.Load() >= 2
	})
}
