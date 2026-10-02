package eventbus_test

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// captureLogs redirects the standard logger and returns a reader of what it
// collected.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var logged strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&lockedWriter{mu: &mu, w: &logged})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return func() string { mu.Lock(); defer mu.Unlock(); return logged.String() }
}

// The consumer's case end to end against a real server: a bus closed by its
// own process at shutdown (Swarm's SIGTERM on a retired blue/green colour)
// must not report "closed permanently" — that went to Sentry once per
// eventbus bundle per deploy — nor log the link as LOST.
func TestNatsBus_CloseAtShutdownIsNotAnError(t *testing.T) {
	dsn := startEmbeddedNATS(t)
	text := captureLogs(t)

	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: dsn, DurablePrefix: "shutdown"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(text(), "connection closed (shutdown)") {
		t.Fatalf("Close did not say it closed:\n%s", text())
	}
	// The connection's callbacks are asynchronous: give any that were going to
	// run the time to run, then require that none did.
	time.Sleep(500 * time.Millisecond)
	got := text()
	if strings.Contains(got, "closed permanently") || strings.Contains(got, "for good") {
		t.Errorf("a requested close was reported as a failure:\n%s", got)
	}
	if strings.Contains(got, "connection LOST") {
		t.Errorf("a requested close was logged as a lost link:\n%s", got)
	}
}

// …and the other side of the same line: a close the BROKER causes is still
// heard. Silencing our own close must not silence a real outage.
func TestNatsBus_ABrokerOutageIsStillHeard(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready")
	}
	text := captureLogs(t)

	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: srv.ClientURL(), DurablePrefix: "outage"})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	srv.Shutdown()
	waitFor(t, 5*time.Second, "a broker outage was not logged", func() bool {
		return strings.Contains(text(), "connection LOST")
	})
}
