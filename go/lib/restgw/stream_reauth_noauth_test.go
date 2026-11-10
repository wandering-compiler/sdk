package restgw_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// A surface with no auth method wires NoAuth, which always succeeds;
// WatchStreamAuth MUST treat it as it treats nil — no watchdog goroutine,
// the context returned unchanged — rather than poll it for every stream.
func TestWatchStreamAuth_NoAuthStartsNoWatchdog(t *testing.T) {
	ctx := context.Background()
	got, stop := restgw.WatchStreamAuth(ctx, restgw.NoAuth(), reauthReq(), time.Millisecond)
	defer stop()
	if got != ctx {
		t.Fatal("WatchStreamAuth(NoAuth()) built a watched context; want ctx unchanged")
	}
	if !restgw.IsNoAuth(restgw.NoAuth()) {
		t.Fatal("IsNoAuth(NoAuth()) = false")
	}
	other := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) { return nil, nil })
	if restgw.IsNoAuth(other) || restgw.IsNoAuth(nil) {
		t.Fatal("IsNoAuth is true for an AuthFunc that is not NoAuth")
	}
}
