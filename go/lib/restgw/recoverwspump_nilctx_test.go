package restgw_test

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/coder/websocket"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestRecoverWSPump_NilContextFallback pins the nil-ctx → Background
// fallback arm: a pump panic recovered with a nil context still reports
// and closes the conn (the existing test passes a non-nil ctx).
func TestRecoverWSPump_NilContextFallback(t *testing.T) {
	orig := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)

	wsRoundTrip(t, nil,
		func(_ context.Context, conn *websocket.Conn) {
			//nolint:staticcheck // SA1012: deliberately exercising the nil-ctx fallback.
			defer restgw.RecoverWSPump(nil, conn, "Svc.Pump")
			panic("pump exploded")
		},
		func(ctx context.Context, conn *websocket.Conn) {
			if _, _, err := conn.Read(ctx); err == nil {
				t.Error("expected conn closed after a nil-ctx pump panic")
			}
		})
}
