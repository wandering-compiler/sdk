package restgw_test

// Replica pin for the shared SSE hub (B-F2).
//
// A gateway is deployed as N pods; every pod runs the SAME code and computes
// the SAME bus options, and each pod holds a DIFFERENT set of browser
// connections. So an event that reaches only one pod is silently missed by
// every client connected to the others — including `w17.invalidate`, which
// leaves those clients showing stale data with no error anywhere.
//
// This is written against the REAL NatsBus + a real (embedded) JetStream
// broker, with the option shape the generated gateway's serve.go builds, so it
// fails if either the adapter or the generated wiring stops giving each
// replica its own consumer identity. The generator side is pinned separately
// (srcgo/domains/gateway/generator).

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// hubReplica is one gateway pod: its own bus + its own hub + one connected SSE
// client.
type hubReplica struct {
	bus    *eventbus.NatsBus
	frames <-chan restgw.Event
}

// startHubReplica builds a replica exactly as the generated gateway does —
// same DurablePrefix (it is derived from the env prefix, a compile-time
// constant, so every replica computes the identical value) and the fan-out
// delivery the SSE hub needs.
func startHubReplica(t *testing.T, dsn, channel, topic string) *hubReplica {
	t.Helper()
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           dsn,
		DurablePrefix: "APP_GATEWAY-w17events",
		Delivery:      eventbus.DeliveryFanOut,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bus.Close(ctx)
	})

	src := restgw.NewEventbusEventSource(bus, []string{channel}, hubTranscode, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	frames, err := src.Subscribe(ctx, []string{topic}, nil)
	if err != nil {
		t.Fatalf("hub Subscribe: %v", err)
	}
	return &hubReplica{bus: bus, frames: frames}
}

// INVARIANT (B-F2): with two hub replicas on one channel, ONE published event
// reaches the connected client of BOTH. Competing-consumer delivery — one
// durable / one consumer group shared by every replica — gives it to exactly
// one of them, so roughly half of all events vanish at N=2, nondeterministic
// and silent.
func TestEventbusEventSource_EveryReplicaReceivesEveryEvent(t *testing.T) {
	dsn := startEmbeddedNATS(t)

	r1 := startHubReplica(t, dsn, "tasks", "task.created")
	r2 := startHubReplica(t, dsn, "tasks", "task.created")

	// Emitted by a third process, as in production (the storage tier emits;
	// the gateway replicas only consume).
	producer, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           dsn,
		DurablePrefix: "APP_STORAGE-subscribers",
	})
	if err != nil {
		t.Fatalf("NewNatsBus(producer): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = producer.Close(ctx)
	})
	if err := producer.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String("hello")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	for i, r := range []*hubReplica{r1, r2} {
		select {
		case ev := <-r.frames:
			if ev.Topic != "task.created" {
				t.Fatalf("replica %d: got topic %q, want task.created", i+1, ev.Topic)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("replica %d's SSE client never received the event — every client "+
				"connected to that replica silently missed it", i+1)
		}
	}
}
