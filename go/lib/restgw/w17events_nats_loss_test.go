package restgw_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The SSE hub on a NATS bus keeps delivering to a connected client after
// the broker loses the channel's stream: the hub's own subscriber
// re-creates its consumer, and an event emitted after the loss reaches
// the client on the same connection.
func TestEventbusEventSource_ResumesAfterTheBrokerLosesTheStream_Embedded(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	bus, err := eventbus.NewNatsBus(eventbus.NatsBusOptions{DSN: srv.ClientURL(), DurablePrefix: "hub", PublishTimeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("NewNatsBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	source := restgw.NewEventbusEventSource(bus, []string{"hubch"}, rawTranscode, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, source, time.Hour)
	}))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/w17-events?topics=a.b", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("w17-events = %d %s", resp.StatusCode, b)
	}
	frames := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			// rawTranscode sends the envelope bytes, which may hold a
			// newline: every line is a candidate.
			frames <- sc.Text()
		}
		close(frames)
	}()
	await := func(want string) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatalf("stream ended before %q arrived", want)
				}
				if strings.Contains(f, want) {
					return
				}
			case <-deadline:
				t.Fatalf("%q never reached the SSE client", want)
			}
		}
	}

	// The hub opens its subscriber with the first client; emit until it sees one.
	for i := 0; ; i++ {
		if err := bus.Dispatch(ctx, "hubch", "a.b", wrapperspb.String("before")); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		select {
		case f := <-frames:
			if strings.Contains(f, "before") {
				goto connected
			}
		case <-time.After(200 * time.Millisecond):
		}
		if i > 50 {
			t.Fatal("the hub never delivered an event before the loss")
		}
	}
connected:
	nc, err := natsgo.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	if err := js.DeleteStream(ctx, "hubch"); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	if err := bus.Dispatch(ctx, "hubch", "a.b", wrapperspb.String("after-loss")); err != nil {
		t.Fatalf("Dispatch after the loss: %v", err)
	}
	await("after-loss")
}
