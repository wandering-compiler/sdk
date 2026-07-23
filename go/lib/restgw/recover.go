// HTTP-side panic recovery (REV-032 Cat 4 sweep, F9). Mirrors
// the gRPC `lib/grpcerr.RecoverPanic` discipline at the
// gateway's top middleware layer: any panic in a downstream
// middleware OR handler converts to HTTP 500 with the standard
// error envelope; the panic value + stack trace go to
// `lib/observx.ReportError` (Sentry + OTel span tagged + log
// fallback). The panic VALUE never escapes to the client —
// envelope message stays generic ("internal error") matching
// Phase C principle 2.
//
// Wrap order: OUTERMOST in the gateway chain, before anything
// else. A panic in CORS / Observability / handler all surface
// uniformly. Without this wrap a downstream panic would propagate
// to net/http's default `Server.recoverPanic` which logs to its
// ErrorLog (often /dev/null) and silently drops the response —
// the client sees a connection close and ops sees nothing.

package restgw

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/coder/websocket"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// RecoverPanicMiddleware wraps `next` with a deferred panic
// recovery. On panic:
//
//  1. debug.Stack() + the panic value get formatted into an
//     error and routed through observx.ReportError(ctx, err)
//     — Sentry event with service tags + active OTel span
//     gets the error attached.
//  2. Response: HTTP 500 with the standard restgw envelope
//     (`{"error":{"code":"INTERNAL","message":"internal
//     error"}}`). When the panic fires AFTER WriteHeader has
//     already been called the response is corrupted regardless
//     — best-effort write + log are the only options.
//
// No-op when no panic in flight.
func RecoverPanicMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the standard sentinel
			// net/http uses for "the handler intentionally
			// aborted" (e.g. hijack patterns). Re-panic so
			// the outer Server's default handling kicks in
			// — logging this would be noise.
			if rec == http.ErrAbortHandler { //nolint:errorlint // rec is a recovered panic value (any); identity compare to the sentinel is correct.
				panic(rec)
			}
			ctx := r.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			observx.ReportError(ctx, fmt.Errorf("PANIC %s %s: %v\n%s",
				r.Method, r.URL.Path, rec, debug.Stack()))
			// Best-effort write — if the handler already
			// flushed any bytes the response is mangled
			// anyway and there's nothing better we can do
			// from here.
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// RecoverWSPump is the deferred panic recovery for a WebSocket
// read/write pump goroutine — the bidi/client-stream reader that
// generated WS handlers spawn OFF the request goroutine. A panic
// there is invisible to RecoverPanicMiddleware (different goroutine),
// so without this the whole gateway process crashes, dropping every
// concurrent connection. On panic it routes value + stack through
// observx (Sentry + OTel; a panic is always "not noise") and closes
// the connection with an internal-error status — the panic value
// never reaches the client. Generated pumps emit
// `defer restgw.RecoverWSPump(ctx, conn, "Service.Method")` as the
// first statement of the spawned goroutine. No-op without a panic.
func RecoverWSPump(ctx context.Context, conn *websocket.Conn, method string) {
	rec := recover()
	if rec == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	observx.ReportError(ctx, fmt.Errorf("PANIC WS pump %s: %v\n%s", method, rec, debug.Stack()))
	_ = conn.Close(websocket.StatusInternalError, "internal error")
}
