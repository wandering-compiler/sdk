package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// syncRecorder is an httptest.ResponseRecorder whose body can be read while
// the handler is still writing.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{rec: httptest.NewRecorder()} }

func (s *syncRecorder) Header() http.Header { return s.rec.Header() }
func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}
func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}
func (s *syncRecorder) Flush() {}
func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

// eventsUnderReauth runs the w17-events route exactly as the generated
// gateway mounts it — RequireAuth, then the re-auth watchdog around
// HandleW17Events — against a memory bus whose events are labelled
// {project_id: alpha}. The principal's labels are the part of its AuthResp
// before any "|", so two verdicts can differ in bytes but not in labels.
//
// It returns a channel closed when the handler returns, the response
// recorder, and a function that dispatches one event.
func eventsUnderReauth(t *testing.T, authFn restgw.AuthFunc) (<-chan struct{}, *syncRecorder, func()) {
	t.Helper()
	bus := eventbus.NewMemoryBus()
	transcode := func(_ string, envelope []byte) ([]byte, map[string]string, bool, error) {
		return envelope, map[string]string{"project_id": "alpha"}, false, nil
	}
	principal := func(userData []byte) map[string]string {
		if len(userData) == 0 {
			return nil
		}
		id, _, _ := strings.Cut(string(userData), "|")
		return map[string]string{"project_id": id}
	}
	source := restgw.NewEventbusEventSource(bus, []string{"default"}, transcode, principal)

	handler := restgw.RequireAuth(authFn, func(w http.ResponseWriter, r *http.Request) {
		ctx, stop := restgw.WatchStreamAuth(r.Context(), authFn, r, 5*time.Millisecond)
		defer stop()
		restgw.HandleW17Events(w, r.WithContext(ctx), source, time.Hour)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodGet, "/v1/w17-events?topics=p.evt", nil).WithContext(ctx)
	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() { handler(rec, req); close(done) }()
	dispatch := func() {
		_ = bus.Dispatch(context.Background(), "default", "p.evt", wrapperspb.String("x"))
	}
	return done, rec, dispatch
}

func endsWithin(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// A credential revoked while the hub stream is open ends it. The hub is the
// longest-lived stream a surface has; authenticating it only at connect let
// a revoked credential keep receiving the tenant's events for days.
func TestW17Events_RevokedCredentialEndsTheStream(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		if calls.Add(1) > 3 {
			return nil, status.Error(codes.Unauthenticated, "revoked")
		}
		return []byte("alpha"), nil
	})
	done, _, _ := eventsUnderReauth(t, authFn)
	if !endsWithin(done, 2*time.Second) {
		t.Fatal("w17-events kept streaming after the credential was refused")
	}
}

// A membership removed while the credential stays valid: auth still
// succeeds, but the principal's labels are not the ones it connected with.
// The stream must end — otherwise it keeps receiving the old tenant's
// events on labels frozen at connect.
func TestW17Events_ChangedMembershipEndsTheStream(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		if calls.Add(1) > 3 {
			return []byte("beta"), nil
		}
		return []byte("alpha"), nil
	})
	done, _, _ := eventsUnderReauth(t, authFn)
	if !endsWithin(done, 2*time.Second) {
		t.Fatal("w17-events kept delivering on the labels captured at connect after the principal's membership changed")
	}
}

// The other direction: a principal whose AuthResp bytes change on every
// probe (a refreshed expiry, say) but whose labels do not must keep its
// stream, and keep receiving events.
func TestW17Events_SameLabelsKeepTheStream(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		n := calls.Add(1)
		return []byte("alpha|" + string(rune('a'+n%26))), nil
	})
	done, rec, dispatch := eventsUnderReauth(t, authFn)
	if endsWithin(done, 150*time.Millisecond) {
		t.Fatal("w17-events ended although the principal's labels never changed")
	}
	if calls.Load() < 3 {
		t.Fatalf("watchdog probed %d time(s); the test proves nothing without probes", calls.Load())
	}
	dispatch()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(rec.body(), "event: p.evt\n") {
		if time.Now().After(deadline) {
			t.Fatalf("event never delivered after re-auth; body:\n%s", rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An auth outage must not end the hub stream: the backend answering
// UNAVAILABLE is not a verdict on the credential.
func TestW17Events_AuthOutageKeepsTheStream(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		if calls.Add(1) > 1 {
			return nil, status.Error(codes.Unavailable, "connection refused")
		}
		return []byte("alpha"), nil
	})
	defer restgw.SetStreamReauthRetryMinForTest(time.Millisecond)()
	done, rec, dispatch := eventsUnderReauth(t, authFn)
	if endsWithin(done, 150*time.Millisecond) {
		t.Fatal("an auth-backend outage ended the w17-events stream")
	}
	if calls.Load() < 3 {
		t.Fatalf("watchdog probed %d time(s) during the outage; it must keep asking", calls.Load())
	}
	dispatch()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(rec.body(), "event: p.evt\n") {
		if time.Now().After(deadline) {
			t.Fatalf("event never delivered during the auth outage; body:\n%s", rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
