package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The outermost middleware gives each HTTP request its own error-tracker
// scope, so breadcrumbs one request records never reach another request's
// report.
func TestRecoverPanicMiddleware_GivesEachRequestItsOwnScope(t *testing.T) {
	if err := observx.MustInit(observx.Config{ServiceName: "svc", SentryDSN: "https://abc123@127.0.0.1/1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observx.Shutdown(ctx)
	})

	var hubs []*sentry.Hub
	h := restgw.RecoverPanicMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubs = append(hubs, sentry.GetHubFromContext(r.Context()))
	}))
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if len(hubs) != 2 || hubs[0] == nil || hubs[1] == nil {
		t.Fatalf("a request ran without its own hub: %v", hubs)
	}
	if hubs[0] == hubs[1] || hubs[0] == sentry.CurrentHub() {
		t.Error("requests share a hub")
	}
}
