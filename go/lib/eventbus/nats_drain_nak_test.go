package eventbus_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A delivery that reaches a subscriber after its drain began is handed back
// to the broker at once (Nak), not run and not dropped: another replica on
// the same consumer receives it well inside AckWait.
func TestNatsBus_DeliveryDuringDrainIsHandedBack_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	newBus := func() *eventbus.NatsBus {
		b, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: url, DurablePrefix: "drain-nak", DefaultAckWait: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close(timeout(t, 5*time.Second)) })
		return b
	}

	a := newBus()
	subA, err := a.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var aSaw []string
	if err := subA.Subscribe(context.Background(), "**", func(_ context.Context, topic string, _ []byte) error {
		mu.Lock()
		aSaw = append(aSaw, topic)
		mu.Unlock()
		if topic == "first" {
			close(started)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.Dispatch(context.Background(), "default", "first", wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := a.Dispatch(context.Background(), "default", "second", wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // "second" sits in A's buffer behind "first"
	drained := make(chan error, 1)
	go func() { drained <- subA.Drain(timeout(t, 10*time.Second)) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}

	b := newBus()
	subB, err := b.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 4)
	if err := subB.Subscribe(context.Background(), "**", func(_ context.Context, topic string, _ []byte) error {
		got <- topic
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case topic := <-got:
		if topic != "second" {
			t.Fatalf("replica B received %q, want the delivery A handed back", topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery that arrived during A's drain was not handed back (AckWait is 30s)")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, topic := range aSaw {
		if topic == "second" {
			t.Error("A ran the handler for a delivery that arrived after its drain began")
		}
	}
}
