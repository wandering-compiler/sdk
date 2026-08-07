package eventbus_test

// Delivery-semantics pins for the two production adapters, against the
// embedded brokers (in-process nats-server / miniredis — no Docker, no shared
// stack).
//
// Two properties are pinned here, and they are pinned as a PAIR because the
// whole defect class is one being silently applied where the other was meant:
//
//   - WHO receives a message when a surface runs N replicas
//     (DeliveryQueue = exactly one of them, DeliveryFanOut = all of them);
//   - WHERE a fresh subscriber starts reading
//     (queue = the retained backlog, fan-out = only what arrives afterwards).
//
// Each "replica" here is its own bus object with IDENTICAL options — exactly
// what N pods of one generated service are: same DurablePrefix / GroupPrefix,
// no replica identity in the wiring.

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// replicaBus builds one "replica" of a surface: a bus with the options every
// replica of that surface computes identically.
type replicaBus struct {
	closer func(context.Context) error
	sub    eventbus.Subscriber
	got    chan string
}

// startNatsReplica dials a NATS replica on `dsn` and subscribes it to
// `channel`, recording every delivered topic on the returned channel.
func startNatsReplica(t *testing.T, dsn, channel string, mode eventbus.DeliveryMode) *replicaBus {
	t.Helper()
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           dsn,
		DurablePrefix: "APP_GATEWAY-w17events",
		Delivery:      mode,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	r := &replicaBus{closer: bus.Close, got: make(chan string, 16)}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(context.Background(), channel)
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	r.sub = sub
	if err := sub.Subscribe(context.Background(), "**", r.record()); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return r
}

// startRedisReplica is startNatsReplica's Redis twin.
func startRedisReplica(t *testing.T, dsn, channel string, mode eventbus.DeliveryMode) *replicaBus {
	t.Helper()
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:         dsn,
		GroupPrefix: "APP_GATEWAY-w17events",
		Delivery:    mode,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	r := &replicaBus{closer: bus.Close, got: make(chan string, 16)}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(context.Background(), channel)
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	r.sub = sub
	if err := sub.Subscribe(context.Background(), "**", r.record()); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return r
}

func (r *replicaBus) record() eventbus.HandlerFunc {
	var mu sync.Mutex
	return func(_ context.Context, topic string, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		select {
		case r.got <- topic:
		default:
		}
		return nil
	}
}

// received waits up to budget for one delivery, returning ("", false) on
// timeout.
func (r *replicaBus) received(budget time.Duration) (string, bool) {
	select {
	case topic := <-r.got:
		return topic, true
	case <-time.After(budget):
		return "", false
	}
}

// INVARIANT (B-F2): an SSE hub is a FAN-OUT consumer. Two replicas of one
// surface, wired exactly alike, must BOTH receive every event — each holds a
// different set of browser connections, so a message that reaches only one of
// them is silently missed by the other's clients (including w17.invalidate →
// stale FE caches, no error anywhere).
//
// The queue row is not decoration: it pins the OPPOSITE semantics for a
// durable work queue (exactly one replica processes each message) so a future
// "fix" cannot make everything fan-out.
func TestNatsBus_DeliveryMode_ReplicaFanOut_Embedded(t *testing.T) {
	t.Run("fanout: both replicas receive", func(t *testing.T) {
		dsn := startEmbeddedNATS(t)
		r1 := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)
		r2 := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)

		publish(t, natsPublisher(t, dsn), "tasks", "task.created")

		if _, ok := r1.received(10 * time.Second); !ok {
			t.Error("replica 1 never received the event")
		}
		if _, ok := r2.received(10 * time.Second); !ok {
			t.Error("replica 2 never received the event — each SSE replica holds " +
				"DIFFERENT browser connections, so this is a silently missed event")
		}
	})

	t.Run("queue: exactly one replica receives", func(t *testing.T) {
		dsn := startEmbeddedNATS(t)
		r1 := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryQueue)
		r2 := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryQueue)

		publish(t, natsPublisher(t, dsn), "tasks", "task.created")

		n := 0
		if _, ok := r1.received(5 * time.Second); ok {
			n++
		}
		if _, ok := r2.received(5 * time.Second); ok {
			n++
		}
		if n != 1 {
			t.Fatalf("work-queue delivery reached %d replicas, want exactly 1", n)
		}
	})
}

func TestRedisBus_DeliveryMode_ReplicaFanOut_Embedded(t *testing.T) {
	t.Run("fanout: both replicas receive", func(t *testing.T) {
		dsn := startEmbeddedRedis(t)
		r1 := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)
		r2 := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)

		publish(t, redisPublisher(t, dsn), "tasks", "task.created")

		if _, ok := r1.received(10 * time.Second); !ok {
			t.Error("replica 1 never received the event")
		}
		if _, ok := r2.received(10 * time.Second); !ok {
			t.Error("replica 2 never received the event — each SSE replica holds " +
				"DIFFERENT browser connections, so this is a silently missed event")
		}
	})

	t.Run("queue: exactly one replica receives", func(t *testing.T) {
		dsn := startEmbeddedRedis(t)
		r1 := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryQueue)
		r2 := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryQueue)

		publish(t, redisPublisher(t, dsn), "tasks", "task.created")

		n := 0
		if _, ok := r1.received(5 * time.Second); ok {
			n++
		}
		if _, ok := r2.received(5 * time.Second); ok {
			n++
		}
		if n != 1 {
			t.Fatalf("work-queue delivery reached %d replicas, want exactly 1", n)
		}
	})
}

// INVARIANT (B-F7): the two adapters agree on where a fresh subscriber starts
// reading, and the start position follows the DELIVERY MODE:
//
//   - a work queue starts at the retained backlog, so a surface deployed after
//     its producer does not silently skip everything emitted meanwhile;
//   - a fan-out hub starts at NEW, because it is a live-notification surface —
//     replaying history into a browser presents stale events as live.
//
// Before this pin NATS was DeliverAll in both roles (zero value, never stated)
// and Redis was "$" in both roles: each adapter was right for one role and
// wrong for the other, and nothing said which was intended.
func TestNatsBus_StartPosition_Embedded(t *testing.T) {
	t.Run("queue: the pre-existing backlog is delivered", func(t *testing.T) {
		dsn := startEmbeddedNATS(t)
		publish(t, natsPublisher(t, dsn), "tasks", "task.backlog")

		r := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryQueue)
		topic, ok := r.received(10 * time.Second)
		if !ok || topic != "task.backlog" {
			t.Fatalf("work queue missed the backlog (got %q, ok=%v)", topic, ok)
		}
	})

	t.Run("fanout: only what arrives after the hub attaches", func(t *testing.T) {
		dsn := startEmbeddedNATS(t)
		pub := natsPublisher(t, dsn)
		publish(t, pub, "tasks", "task.backlog")

		r := startNatsReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)
		publish(t, pub, "tasks", "task.live")

		topic, ok := r.received(10 * time.Second)
		if !ok {
			t.Fatal("the hub received nothing at all")
		}
		if topic != "task.live" {
			t.Fatalf("hub replayed history: first frame was %q, want task.live", topic)
		}
	})
}

func TestRedisBus_StartPosition_Embedded(t *testing.T) {
	t.Run("queue: the pre-existing backlog is delivered", func(t *testing.T) {
		dsn := startEmbeddedRedis(t)
		publish(t, redisPublisher(t, dsn), "tasks", "task.backlog")

		r := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryQueue)
		topic, ok := r.received(10 * time.Second)
		if !ok || topic != "task.backlog" {
			t.Fatalf("work queue missed the backlog (got %q, ok=%v)", topic, ok)
		}
	})

	t.Run("fanout: only what arrives after the hub attaches", func(t *testing.T) {
		dsn := startEmbeddedRedis(t)
		pub := redisPublisher(t, dsn)
		publish(t, pub, "tasks", "task.backlog")

		r := startRedisReplica(t, dsn, "tasks", eventbus.DeliveryFanOut)
		publish(t, pub, "tasks", "task.live")

		topic, ok := r.received(10 * time.Second)
		if !ok {
			t.Fatal("the hub received nothing at all")
		}
		if topic != "task.live" {
			t.Fatalf("hub replayed history: first frame was %q, want task.live", topic)
		}
	})
}

// natsPublisher / redisPublisher build the producing side — a separate bus, as
// in production (the emitting service is not the subscribing one).
func natsPublisher(t *testing.T, dsn string) eventbus.Dispatcher {
	t.Helper()
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "producer"})
	if err != nil {
		t.Fatalf("NewNatsBus(producer): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	return bus
}

func redisPublisher(t *testing.T, dsn string) eventbus.Dispatcher {
	t.Helper()
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: dsn, GroupPrefix: "producer"})
	if err != nil {
		t.Fatalf("NewRedisBus(producer): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	return bus
}

func publish(t *testing.T, d eventbus.Dispatcher, channel, topic string) {
	t.Helper()
	if err := d.Dispatch(context.Background(), channel, topic, wrapperspb.String(topic)); err != nil {
		t.Fatalf("Dispatch(%s/%s): %v", channel, topic, err)
	}
}
