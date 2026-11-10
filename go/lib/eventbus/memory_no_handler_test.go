package eventbus_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// An in-process dispatch that matches no handler reaches nobody. It is a
// successful emit (nothing failed), but it must not be untraced: the
// Observer hears OnDrop(no_handler) for each such event.
func TestMemoryBus_DispatchWithNoMatchingHandlerIsADrop(t *testing.T) {
	obs := &recordingObserver{}
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{Observer: obs})

	// No subscriber at all.
	if err := bus.Dispatch(context.Background(), "default", "user.created", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// A subscriber whose filter does not match.
	sub, _ := bus.Subscriber(context.Background(), "other")
	if err := sub.Subscribe(context.Background(), "billing.*", func(context.Context, string, []byte) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Dispatch(context.Background(), "other", "user.created", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// A matching subscriber: no drop.
	if err := bus.Dispatch(context.Background(), "other", "billing.paid", wrapperspb.String("x")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	obs.mu.Lock()
	drops := append([]callRecord(nil), obs.drop...)
	obs.mu.Unlock()
	if len(drops) != 2 {
		t.Fatalf("drops = %+v, want two no_handler drops (no subscriber, no matching filter)", drops)
	}
	for _, d := range drops {
		if d.Reason != eventbus.DropReasonNoHandler || d.Topic != "user.created" {
			t.Errorf("drop = %+v, want reason %q for topic user.created", d, eventbus.DropReasonNoHandler)
		}
	}
}

// The default observer logs a no-handler drop once per channel and topic:
// it is a deployment shape (nothing subscribes in this process), not a
// fault per event, so it is not reported to the error reporter on every
// emit.
func TestDefaultObserver_NoHandlerDropIsLoggedOncePerTopic(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	bus := eventbus.NewMemoryBus()
	for range 3 {
		_ = bus.Dispatch(context.Background(), "nohandler_once", "user.created", wrapperspb.String("x"))
	}
	if got := strings.Count(buf.String(), "nohandler_once"); got != 1 {
		t.Fatalf("no-handler drop logged %d times for one channel and topic, want once:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "user.created") {
		t.Errorf("the line does not name the topic: %q", buf.String())
	}
}
