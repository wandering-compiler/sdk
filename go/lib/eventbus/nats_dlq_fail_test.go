package eventbus_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// When the dead-letter write itself fails, the message is not reported as
// dropped: OnDrop fires only once the payload is in the DLQ, and the failed
// write surfaces as OnDeliverFailure instead, with the message left on its
// stream rather than acked into the void.
func TestNatsBus_FailedDLQWriteIsAFailureNotADrop_Embedded(t *testing.T) {
	url := startEmbeddedNATS(t)
	obs := &recordingObserver{}
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:               url,
		DurablePrefix:     "dlq-down",
		DefaultMaxDeliver: 1,
		// Long enough that the bus's answer to the failed DLQ write lands
		// inside the delivery window, where an Ack would show on the
		// consumer and a Nak does not.
		DefaultAckWait: 10 * time.Second,
		Observer:       obs,
	})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	sub, err := bus.Subscriber(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error {
		return errors.New("handler fails")
	}); err != nil {
		t.Fatal(err)
	}

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteStream(context.Background(), "default_dlq"); err != nil {
		t.Fatalf("delete the DLQ stream: %v", err)
	}

	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("doomed")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "an OnDeliverFailure for the DLQ write", func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		for _, f := range obs.deliverFailure {
			if !strings.Contains(f.Err, "handler fails") {
				return true
			}
		}
		return false
	})
	time.Sleep(time.Second) // room for a wrong OnDrop to show up
	if _, _, _, _, drops := obs.snapshots(); drops != 0 {
		t.Fatalf("OnDrop fired %d times while the DLQ could not be written", drops)
	}
	// Not acked: the consumer's ack floor has not moved past it.
	src, err := js.Stream(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	lister := src.ListConsumers(context.Background())
	n := 0
	for ci := range lister.Info() {
		n++
		if ci.AckFloor.Stream != 0 {
			t.Errorf("consumer %q acked the message (ack floor %d) although the DLQ write failed", ci.Name, ci.AckFloor.Stream)
		}
	}
	if err := lister.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no consumer on the stream")
	}
}
