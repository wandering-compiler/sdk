package eventbus_test

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A subscriber whose consumer disappears server-side must SAY so.
//
// A consumer reported half an hour lost to the silent version: an ordinary
// `docker compose up -d` recreated their broker, delivery stopped, and the
// subscriber's log held one line from before the restart and nothing after it.
// Sign-in returned 200, the challenge row appeared, and the second-factor code
// never arrived — every layer looked healthy.
//
// Recreating a broker is destroying its JetStream state, so this test does the
// same thing to the same effect: it deletes the consumer out from under a live
// Consume loop. Before the ConsumeErrHandler this produced no output at all.
func TestNatsConsumeFailureIsLogged(t *testing.T) {
	dsn := startEmbeddedNATS(t)

	var mu sync.Mutex
	var logged strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&lockedWriter{mu: &mu, w: &logged})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	const channel = "events"
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "silentloss"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	sub, err := bus.Subscriber(context.Background(), channel)
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	if err := sub.Subscribe(context.Background(), "**",
		func(context.Context, string, []byte) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Delete the consumer the way a recreated broker does: it is simply not
	// there any more, and the client finds out on its next pull.
	nc, err := nats.Connect(dsn)
	if err != nil {
		t.Fatalf("probe connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("probe jetstream: %v", err)
	}
	st, err := js.Stream(context.Background(), channel)
	if err != nil {
		t.Fatalf("probe stream: %v", err)
	}
	names := st.ConsumerNames(context.Background())
	var deleted int
	for name := range names.Name() {
		if derr := st.DeleteConsumer(context.Background(), name); derr == nil {
			deleted++
		}
	}
	if deleted == 0 {
		t.Fatal("no consumer to delete — this test would pass without exercising anything")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := logged.String()
		mu.Unlock()
		if strings.Contains(got, "eventbus consume failed") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	got := logged.String()
	mu.Unlock()
	t.Fatalf("a consumer that vanished server-side produced NO log line — a subscriber that "+
		"cannot pull is then indistinguishable from one with nothing to do, which is the "+
		"half-hour this costs. log was:\n%s", got)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
