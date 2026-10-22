package eventbus_test

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// freePort returns a TCP port nothing listens on (yet).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func runNATSOn(t *testing.T, port int, token string) {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = port
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.Authorization = token
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
}

// A broker started in the same deploy comes up after the process that needs
// it (Swarm has no depends_on). The bus waits for it instead of failing the
// boot — the 2026-10-22 console deploy rolled back on exactly that.
func TestNewNatsBus_WaitsForABrokerThatStartsLate(t *testing.T) {
	port := freePort(t)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		runNATSOn(t, port, "")
	}()
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           "nats://127.0.0.1:" + strconv.Itoa(port),
		DurablePrefix: "late",
		ConnectWait:   15 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewNatsBus gave up on a broker that came up 1.5s later: %v", err)
	}
	_ = bus.Close(t.Context())
}

// The wait is bounded, and the error it ends with names the DSN without the
// token a bare userinfo carries.
func TestNewNatsBus_GivesUpAfterConnectWait_WithoutTheToken(t *testing.T) {
	port := freePort(t)
	start := time.Now()
	_, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           "nats://s3cret-token@127.0.0.1:" + strconv.Itoa(port),
		DurablePrefix: "never",
		ConnectWait:   time.Second,
	})
	if err == nil {
		t.Fatal("expected a connect failure, nothing listens")
	}
	if took := time.Since(start); took < time.Second || took > 10*time.Second {
		t.Errorf("gave up after %s, want just past ConnectWait (1s)", took)
	}
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Errorf("the token is in the error: %v", err)
	}
	if !strings.Contains(err.Error(), "nats://xxxxx@127.0.0.1") {
		t.Errorf("the error should still name the broker: %v", err)
	}
}

// A broker that answers and refuses gives the same answer a second later:
// no waiting, whatever ConnectWait says.
func TestNewNatsBus_RefusedTokenFailsAtOnce(t *testing.T) {
	port := freePort(t)
	runNATSOn(t, port, "the-right-token")
	start := time.Now()
	_, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{
		DSN:           "nats://the-wrong-token@127.0.0.1:" + strconv.Itoa(port),
		DurablePrefix: "refused",
		ConnectWait:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an authorization failure")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a refused token was retried for %s: %v", took, err)
	}
	if strings.Contains(err.Error(), "the-wrong-token") {
		t.Errorf("the token is in the error: %v", err)
	}
}
