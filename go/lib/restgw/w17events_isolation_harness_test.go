package restgw_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// isolationHub is a hub behind a real HTTP server, for the audience tests
// that assert an event does NOT arrive. Such a test proves nothing unless
// the client was subscribed and the event was fanned out; the harness gives
// both:
//
//   - a client is registered with the hub before HandleW17Events writes the
//     response headers, so openClient returns only once it is subscribed;
//   - the bus runs one handler at a time (a buffer of 1 on the channel), so
//     an event dispatched after the denied one is fanned out only after the
//     denied one was, and a client's frames arrive in fan-out order. Once a
//     control event dispatched after the denied one has arrived, the denied
//     one either arrived before it or was withheld.
type isolationHub struct {
	bus *eventbus.MemoryBus
	srv *httptest.Server
}

// controlTopic is the control event a principal can receive: labelled
// {project_id: <principal>} by the harness transcode.
func controlTopic(principal string) string { return "ctl." + principal }

func newIsolationHub(t *testing.T, eventLabels map[string]string, principal restgw.PrincipalLabelsFunc) *isolationHub {
	t.Helper()
	bus := eventbus.NewMemoryBusWithOptions(eventbus.MemoryBusOptions{
		BufferSizes:  map[string]int{"default": 1},
		BlockTimeout: 10 * time.Second,
	})
	transcode := func(topic string, envelope []byte) ([]byte, map[string]string, bool, error) {
		if p, ok := strings.CutPrefix(topic, "ctl."); ok {
			return envelope, map[string]string{"project_id": p}, false, nil
		}
		return envelope, eventLabels, false, nil
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, principal)
	authFn := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		return []byte(r.Header.Get("X-Test-Principal")), nil
	})
	srv := httptest.NewServer(restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, source, time.Hour)
	}))
	t.Cleanup(srv.Close)
	return &isolationHub{bus: bus, srv: srv}
}

// openClient connects a principal subscribed to p.evt and its control
// topic (none for the empty principal), and returns the stream of event names it receives. It returns
// once the hub has registered the client.
func (h *isolationHub) openClient(t *testing.T, principal string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/v1/w17-events?topics=p.evt,"+controlTopic(principal), nil)
	req.Header.Set("X-Test-Principal", principal)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open /w17-events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open /w17-events: status %d", resp.StatusCode)
	}
	events := make(chan string, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				events <- name
			}
		}
	}()
	return events
}

func (h *isolationHub) dispatch(t *testing.T, topic string) {
	t.Helper()
	if err := h.bus.Dispatch(context.Background(), "default", topic, wrapperspb.String("x")); err != nil {
		t.Fatalf("dispatch %s: %v", topic, err)
	}
}

// receivedBeforeControl reads events until the principal's control topic
// arrives and reports whether p.evt came first. It fails the test when the
// control never arrives: then the client was not being delivered to at all.
func receivedBeforeControl(t *testing.T, events <-chan string, principal string) bool {
	t.Helper()
	got := false
	timeout := time.After(10 * time.Second)
	for {
		select {
		case name := <-events:
			if name == controlTopic(principal) {
				return got
			}
			if name == "p.evt" {
				got = true
			}
		case <-timeout:
			t.Fatal("the control event never arrived: the client was not subscribed, so the negative assertion would be vacuous")
		}
	}
}

// arrivesWithin reports whether p.evt arrives on events within d.
func arrivesWithin(events <-chan string, d time.Duration) bool {
	timeout := time.After(d)
	for {
		select {
		case name := <-events:
			if name == "p.evt" {
				return true
			}
		case <-timeout:
			return false
		}
	}
}
