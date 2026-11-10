package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// sdkHubServer serves the SDK's own event hub (restgw.HandleW17Events over
// an EventbusEventSource with label isolation) instead of a stub: a bearer
// token is the principal's organization, and POST /api/v1/tasks emits
// task.created labelled with the caller's organization.
func sdkHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	bus := eventbus.NewMemoryBus()
	transcode := func(_ string, envelope []byte) ([]byte, map[string]string, bool, error) {
		var v wrapperspb.StringValue
		if err := proto.Unmarshal(envelope, &v); err != nil {
			return nil, nil, false, err
		}
		var body struct {
			TaskID string `json:"task_id"`
			Org    string `json:"org"`
		}
		if err := json.Unmarshal([]byte(v.GetValue()), &body); err != nil {
			return nil, nil, false, err
		}
		return []byte(v.GetValue()), map[string]string{"org": body.Org}, false, nil
	}
	principal := func(userData []byte) map[string]string {
		if len(userData) == 0 {
			return nil
		}
		return map[string]string{"org": string(userData)}
	}
	source := restgw.NewEventbusEventSource(bus, []string{"tasks"}, transcode, principal)
	authFn := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		return []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), nil
	})
	var n atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/w17-events", restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
		restgw.HandleW17Events(w, r, source, time.Hour)
	}))
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		org := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		id := fmt.Sprintf("t-%d", n.Add(1))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": id})
		go func() {
			time.Sleep(20 * time.Millisecond)
			body, _ := json.Marshal(map[string]string{"task_id": id, "org": org})
			_ = bus.Dispatch(context.Background(), "tasks", "task.created", wrapperspb.String(string(body)))
		}()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The runner's absent await against the SDK hub itself, not a stub: another
// organization subscribed through restgw.HandleW17Events is not shown the
// event (the absent await passes), and the caller's own organization is (the
// absent await fails). Together they prove the absent assertion and the
// hub's label isolation meet as the matrix row says.
func TestAwaitEvent_AbsentThroughTheSDKHub(t *testing.T) {
	srv := sdkHubServer(t)
	scope := sessionScope()
	scope.Capture("bob.token", "bob")

	other := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 500,
		Absent: true, TokenFrom: "bob.token"})
	other.Endpoint.AuthRequired = true
	if err := runWith(t, srv, scope, other); err != nil {
		t.Fatalf("another organization's silence through the SDK hub was not accepted: %v", err)
	}

	self := createTaskStep(&AwaitEvent{Topic: "task.created", Path: "/api/v1/w17-events", TimeoutMs: 2000, Absent: true})
	self.Endpoint.AuthRequired = true
	err := runWith(t, srv, scope, self)
	if err == nil || !strings.Contains(err.Error(), "WAS delivered") {
		t.Fatalf("an event the SDK hub delivered to the caller's organization passed an absent assertion: %v", err)
	}
}
