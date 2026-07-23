package eventbus_test

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// TestEmitInvalidate_DispatchesOnReservedTopic — the helper
// fires Dispatcher.Dispatch with the reserved topic name +
// a populated InvalidateEvent envelope.
func TestEmitInvalidate_DispatchesOnReservedTopic(t *testing.T) {
	bus := &capturingDispatcher{}
	err := eventbus.EmitInvalidate(context.Background(), bus, "default", "accounts", "Account", []string{"acc-1", "acc-2"})
	if err != nil {
		t.Fatalf("EmitInvalidate: %v", err)
	}
	if bus.channel != "default" {
		t.Errorf("channel = %q, want default", bus.channel)
	}
	if bus.topic != eventbus.W17InvalidateTopic {
		t.Errorf("topic = %q, want %q", bus.topic, eventbus.W17InvalidateTopic)
	}
	ev, ok := bus.envelope.(*w17pb.InvalidateEvent)
	if !ok {
		t.Fatalf("envelope = %T, want *w17pb.InvalidateEvent", bus.envelope)
	}
	if ev.GetGroup() != "accounts" || ev.GetMessage() != "Account" {
		t.Errorf("envelope (group, message) = (%q, %q); want (accounts, Account)", ev.GetGroup(), ev.GetMessage())
	}
	if len(ev.GetIds()) != 2 || ev.GetIds()[0] != "acc-1" || ev.GetIds()[1] != "acc-2" {
		t.Errorf("ids = %v; want [acc-1 acc-2]", ev.GetIds())
	}
}

// TestEmitInvalidate_NilIdsAllowed — calling without ids
// (broad invalidate signal, family-level) still emits a
// well-formed envelope.
func TestEmitInvalidate_NilIdsAllowed(t *testing.T) {
	bus := &capturingDispatcher{}
	if err := eventbus.EmitInvalidate(context.Background(), bus, "default", "accounts", "Account", nil); err != nil {
		t.Fatalf("EmitInvalidate: %v", err)
	}
	ev, _ := bus.envelope.(*w17pb.InvalidateEvent)
	if ev == nil || len(ev.GetIds()) != 0 {
		t.Errorf("envelope = %v; want empty ids", ev)
	}
}

// TestEmitInvalidate_EndToEndAgainstMemoryBus — the helper
// drives a real MemoryBus + a registered subscriber receives
// the wire payload + can unmarshal it back.
func TestEmitInvalidate_EndToEndAgainstMemoryBus(t *testing.T) {
	bus := eventbus.NewMemoryBus()
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	var (
		mu          sync.Mutex
		gotTopic    string
		gotEnvelope []byte
	)
	done := make(chan struct{})
	err = sub.Subscribe(context.Background(), "**", func(_ context.Context, topic string, envelope []byte) error {
		mu.Lock()
		defer mu.Unlock()
		gotTopic = topic
		gotEnvelope = append([]byte(nil), envelope...)
		close(done)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := eventbus.EmitInvalidate(context.Background(), bus, "default", "accounts", "Account", []string{"x-1"}); err != nil {
		t.Fatalf("EmitInvalidate: %v", err)
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	if gotTopic != eventbus.W17InvalidateTopic {
		t.Errorf("topic = %q, want %q", gotTopic, eventbus.W17InvalidateTopic)
	}
	var ev w17pb.InvalidateEvent
	if err := proto.Unmarshal(gotEnvelope, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.GetGroup() != "accounts" || ev.GetMessage() != "Account" || len(ev.GetIds()) != 1 || ev.GetIds()[0] != "x-1" {
		t.Errorf("received envelope = %+v; want {accounts, Account, [x-1]}", &ev)
	}
}

// capturingDispatcher records the last Dispatch call for
// assertion. Implements eventbus.Dispatcher.
type capturingDispatcher struct {
	channel  string
	topic    string
	envelope proto.Message
}

func (c *capturingDispatcher) Dispatch(_ context.Context, channel, topic string, envelope proto.Message) error {
	c.channel = channel
	c.topic = topic
	c.envelope = envelope
	return nil
}
