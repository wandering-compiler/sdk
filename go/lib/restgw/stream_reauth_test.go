package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func reauthReq() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/public/notes/tok-abc/live", nil)
}

// waitCancelled reports whether ctx is done within the deadline. Polling
// beats a fixed sleep: the watchdog's goroutine and the test race, and a
// sleep long enough to be reliable is long enough to be slow.
func waitCancelled(ctx context.Context, within time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(within):
		return false
	}
}

// The point of the whole file: a credential that stops authenticating tears
// the stream down instead of feeding it until the socket happens to drop.
func TestWatchStreamAuth_CancelsWhenAuthStartsFailing(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		if calls.Add(1) >= 2 {
			return nil, errors.New("token revoked")
		}
		return nil, nil
	})

	ctx, stop := restgw.WatchStreamAuth(context.Background(), authFn, reauthReq(), 5*time.Millisecond)
	defer stop()

	if !waitCancelled(ctx, 2*time.Second) {
		t.Fatal("stream context still live after auth began failing; a revoked credential would keep streaming")
	}
}

// A credential that keeps authenticating must NOT be disturbed. Without
// this, "cancels on failure" could be satisfied by cancelling always — a
// watchdog that closes every stream on schedule.
func TestWatchStreamAuth_LeavesAHealthyStreamAlone(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	})

	ctx, stop := restgw.WatchStreamAuth(context.Background(), authFn, reauthReq(), 5*time.Millisecond)
	defer stop()

	if waitCancelled(ctx, 100*time.Millisecond) {
		t.Fatal("watchdog cancelled a stream whose auth kept succeeding")
	}
	if calls.Load() == 0 {
		t.Error("watchdog never re-authenticated; the test above would pass for a watchdog that does nothing at all")
	}
}

// A hung or unreachable auth backend must not be read as "revoked".
// Otherwise a backend outage disconnects every live stream on the surface
// at once, turning a partial failure into a total one.
func TestWatchStreamAuth_ProbeTimeoutIsNotARevocation(t *testing.T) {
	authFn := restgw.AuthFunc(func(ctx context.Context, _ *http.Request) ([]byte, error) {
		<-ctx.Done() // never answers; the probe's own timeout ends it
		return nil, ctx.Err()
	})

	ctx, stop := restgw.WatchStreamAuth(context.Background(), authFn, reauthReq(), 5*time.Millisecond)
	defer stop()

	if waitCancelled(ctx, 300*time.Millisecond) {
		t.Fatal("a hung auth backend cancelled the stream; an unanswered probe is not a verdict")
	}
}

// The auth call must not inherit the stream's context. If it did, the
// probe would be cancelled by the very shutdown it exists to be
// independent of — and, worse, an in-flight probe would fail with
// "context canceled" and read as a genuine auth failure.
func TestWatchStreamAuth_ProbeDoesNotInheritStreamCancellation(t *testing.T) {
	probeErr := make(chan error, 4)
	authFn := restgw.AuthFunc(func(ctx context.Context, _ *http.Request) ([]byte, error) {
		probeErr <- ctx.Err()
		return nil, nil
	})

	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := restgw.WatchStreamAuth(parent, authFn, reauthReq(), 5*time.Millisecond)
	defer stop()

	select {
	case err := <-probeErr:
		if err != nil {
			t.Fatalf("probe context already carried %v on entry", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never probed")
	}

	// Cancelling the parent must end the watchdog, not leak the goroutine.
	cancelParent()
	if !waitCancelled(ctx, time.Second) {
		t.Fatal("cancelling the parent did not end the watched context")
	}
}

// interval <= 0 is the documented opt-out. It must return the caller's own
// context untouched, so a handler never has to branch.
func TestWatchStreamAuth_DisabledIsAPassThrough(t *testing.T) {
	var calls atomic.Int32
	authFn := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("would revoke")
	})

	parent := context.Background()
	ctx, stop := restgw.WatchStreamAuth(parent, authFn, reauthReq(), 0)
	defer stop()

	if ctx != parent {
		t.Error("disabled watchdog returned a derived context; it must hand back the caller's own")
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("disabled watchdog still called auth %d time(s)", calls.Load())
	}
}

// A surface with no auth methods wires NoAuth, which always succeeds.
// Polling it would burn a goroutine per stream to learn nothing.
func TestWatchStreamAuth_NilAuthIsAPassThrough(t *testing.T) {
	parent := context.Background()
	ctx, stop := restgw.WatchStreamAuth(parent, nil, reauthReq(), time.Millisecond)
	defer stop()
	if ctx != parent {
		t.Error("nil authFn returned a derived context; it must hand back the caller's own")
	}
}

func TestStreamReauthIntervalFromEnv(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	cases := []struct {
		name string
		raw  map[string]string
		want time.Duration
	}{
		// Unset defaults ON. A security fix that ships switched off is a
		// security fix nobody gets — the deployments that most need it are
		// the ones not reading release notes for env vars to add.
		{"unset", nil, restgw.DefaultStreamReauthInterval},
		{"explicit", map[string]string{"APP_STREAM_REAUTH_SECONDS": "30"}, 30 * time.Second},
		{"zero disables", map[string]string{"APP_STREAM_REAUTH_SECONDS": "0"}, 0},
		{"negative disables", map[string]string{"APP_STREAM_REAUTH_SECONDS": "-1"}, 0},
		// A typo in a security knob must not silently disable it.
		{"garbage falls back to the default", map[string]string{"APP_STREAM_REAUTH_SECONDS": "soon"}, restgw.DefaultStreamReauthInterval},
		{"empty falls back to the default", map[string]string{"APP_STREAM_REAUTH_SECONDS": ""}, restgw.DefaultStreamReauthInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restgw.StreamReauthIntervalFromEnv("APP", env(tc.raw)); got != tc.want {
				t.Errorf("interval = %v, want %v", got, tc.want)
			}
		})
	}
	if got := restgw.StreamReauthIntervalFromEnv("APP", nil); got != restgw.DefaultStreamReauthInterval {
		t.Errorf("nil getenv = %v, want the default", got)
	}
}

// TestWatchStreamAuth_TicketStreamSurvivesTheTick — the two features that
// shipped together and cancel each other out.
//
// A ticket is ONE-SHOT by contract: Redeem deletes it. The watchdog probes
// by re-running the surface's auth against the request it was given — and
// for a ticket-authed stream that request still carries only `?ticket=`,
// whose ticket was consumed at the handshake seconds earlier. So the first
// tick redeems nothing, reads ErrTicketInvalid, and cancels a stream whose
// principal was never revoked: every bearer'd browser WS dies at the
// default 300s with no transparent reconnect, and a raw EventSource
// reconnect replays the consumed ticket into a permanent 401.
//
// T2-6 pass #9, B9-9. The stream must survive as long as the credential the
// ticket STOOD FOR is still good — which is what the watchdog is for.
func TestWatchStreamAuth_TicketStreamSurvivesTheTick(t *testing.T) {
	store := restgw.NewMemoryTicketStore()
	ticket, err := store.Issue(context.Background(), map[string]string{
		restgw.WSAuthTicketHeaderLabel: "Bearer still-valid",
	}, time.Minute)
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}

	// Inner is the real credential check: it answers for the bearer the
	// ticket stood for, and never for a request with no Authorization.
	var innerCalls atomic.Int32
	inner := restgw.AuthFunc(func(_ context.Context, r *http.Request) ([]byte, error) {
		innerCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer still-valid" {
			return nil, errors.New("no credential")
		}
		return nil, nil
	})
	authFn := restgw.NewWSAuth(restgw.WSAuthConfig{
		Inner:       inner,
		Modes:       []string{restgw.WSAuthModeHeader, restgw.WSAuthModeTicket},
		TicketStore: store,
	})

	// The handshake: the surface authenticates the upgrade request, which
	// consumes the ticket. This is exactly what serve.go does before the
	// generated handler runs.
	r := httptest.NewRequest(http.MethodGet, "/public/notes/live?ticket="+ticket, nil)
	if _, err := authFn(context.Background(), r); err != nil {
		t.Fatalf("handshake auth: %v", err)
	}

	// The watchdog then gets that same request — the generated handler
	// passes `r`.
	ctx, stop := restgw.WatchStreamAuth(context.Background(), authFn, r, 5*time.Millisecond)
	defer stop()

	if waitCancelled(ctx, 300*time.Millisecond) {
		t.Fatal("a ticket-authed stream was torn down by its own re-auth probe: the ticket was consumed at the handshake, so the probe re-presented a spent credential and read the failure as a revocation")
	}
	if innerCalls.Load() < 2 {
		t.Errorf("the probe never reached the real credential check (inner called %d times) — the watchdog must still be ASKING, not silently skipping", innerCalls.Load())
	}
}
