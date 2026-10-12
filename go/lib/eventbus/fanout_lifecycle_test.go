package eventbus_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A fan-out NATS consumer belongs to one process: it is ephemeral (no
// durable name) and the server reaps it five minutes after it stops pulling,
// so a restarted replica does not leave a consumer behind forever.
func TestNatsBus_FanOutConsumerIsEphemeralAndReaped_Embedded(t *testing.T) {
	dsn := startEmbeddedNATS(t)
	startNatsReplica(t, dsn, "live", eventbus.DeliveryFanOut)

	nc, err := natsgo.Connect(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := js.Stream(timeout(t, 5*time.Second), "live")
	if err != nil {
		t.Fatalf("stream live: %v", err)
	}
	n := 0
	lister := st.ListConsumers(timeout(t, 5*time.Second))
	for info := range lister.Info() {
		n++
		if info.Config.Durable != "" {
			t.Errorf("fan-out consumer %q is durable (%q)", info.Name, info.Config.Durable)
		}
		if info.Config.InactiveThreshold != 5*time.Minute {
			t.Errorf("fan-out consumer %q InactiveThreshold = %v, want 5m", info.Name, info.Config.InactiveThreshold)
		}
	}
	if err := lister.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no consumer on the fan-out channel's stream")
	}
}

// A fan-out Redis group belongs to one process and is destroyed on Close; the
// surface's queue group on the same channel stays.
func TestRedisBus_FanOutGroupDestroyedOnClose_Embedded(t *testing.T) {
	dsn := startEmbeddedRedis(t)
	raw := rawRedisClient(t, dsn)
	startRedisReplica(t, dsn, "live", eventbus.DeliveryQueue)
	fan := startRedisReplica(t, dsn, "live", eventbus.DeliveryFanOut)

	groups := func() []string {
		gs, err := raw.XInfoGroups(context.Background(), "live").Result()
		if err != nil {
			t.Fatalf("XINFO GROUPS: %v", err)
		}
		var names []string
		for _, g := range gs {
			names = append(names, g.Name)
		}
		return names
	}
	if got := groups(); len(got) != 2 {
		t.Fatalf("groups before Close = %v, want the queue group and one fan-out group", got)
	}
	if err := fan.closer(timeout(t, 5*time.Second)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := groups(); len(got) != 1 {
		t.Fatalf("groups after the fan-out bus closed = %v, want only the queue group", got)
	}
}

// After a transient link loss — shorter than the client's reconnect budget —
// a fan-out subscriber receives what was emitted while it was cut off: its
// consumer (NATS) or group (Redis) outlived the disconnect.
func TestFanOut_BacklogSinceDisconnectArrivesAfterReconnect(t *testing.T) {
	t.Run("nats", func(t *testing.T) {
		dsn := startEmbeddedNATS(t)
		proxy := newStallProxy(t, strings.TrimPrefix(dsn, "nats://"))
		r := startNatsReplica(t, "nats://"+proxy.addr, "live", eventbus.DeliveryFanOut)
		producer, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "producer"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = producer.Close(timeout(t, 5*time.Second)) })
		expectBacklogAcrossCut(t, r, proxy, producer)
	})
	t.Run("redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		proxy := newStallProxy(t, mr.Addr())
		// No client-side retries: the outage must reach the consume loop.
		r := startRedisReplica(t, "redis://"+proxy.addr+"?max_retries=-1", "live", eventbus.DeliveryFanOut)
		producer, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: "redis://" + mr.Addr(), GroupPrefix: "producer"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = producer.Close(timeout(t, 5*time.Second)) })
		expectBacklogAcrossCut(t, r, proxy, producer)
	})
}

func expectBacklogAcrossCut(t *testing.T, r *replicaBus, proxy *stallProxy, producer eventbus.Dispatcher) {
	t.Helper()
	if err := producer.Dispatch(context.Background(), "live", "before.cut", wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	if topic, ok := r.received(10 * time.Second); !ok || topic != "before.cut" {
		t.Fatalf("before the cut: got %q, %v", topic, ok)
	}
	proxy.stall()
	proxy.cut()
	for _, topic := range []string{"during.one", "during.two"} {
		if err := producer.Dispatch(context.Background(), "live", topic, wrapperspb.String("x")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	proxy.stalled.Store(false)
	// The backlog is promised, its order is not: a message the server had
	// already handed to the dying connection comes back only after AckWait
	// (30s), behind the ones still queued.
	missing := map[string]bool{"during.one": true, "during.two": true}
	deadline := time.Now().Add(45 * time.Second)
	for len(missing) > 0 {
		topic, ok := r.received(time.Until(deadline))
		if !ok {
			t.Fatalf("emitted during the link loss, never arrived after reconnect: %v", missing)
		}
		delete(missing, topic)
	}
}
