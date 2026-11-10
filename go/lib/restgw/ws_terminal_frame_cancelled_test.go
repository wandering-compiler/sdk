package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The re-auth watchdog ends a socket by cancelling the handler's context,
// and the generated handler then writes the refusal with that same context.
// The bounded terminal writers MUST still deliver the error frame (bounded
// by their timeout), not only the close: the frame is where the client
// reads the code and the full sentence.
func TestWSTerminalFrame_IsWrittenOnACancelledContext(t *testing.T) {
	for name, write := range map[string]func(ctx context.Context, c *websocket.Conn){
		"WSWriteGRPCErrorWithTimeout": func(ctx context.Context, c *websocket.Conn) {
			restgw.WSWriteGRPCErrorWithTimeout(ctx, c, status.Error(codes.PermissionDenied, "revoked"), time.Second)
		},
		"WSWriteErrorWithDetailsWithTimeout": func(ctx context.Context, c *websocket.Conn) {
			restgw.WSWriteErrorWithDetailsWithTimeout(ctx, c, "PERMISSION_DENIED", "revoked", nil, time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				ctx, cancel := context.WithCancel(r.Context())
				cancel() // what the watchdog does
				write(ctx, conn)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.CloseNow() }()
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("no error frame reached the client, only: %v", err)
			}
			if !strings.Contains(string(data), "PERMISSION_DENIED") {
				t.Fatalf("frame = %s, want the PERMISSION_DENIED envelope", data)
			}
			if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("after the frame: %v, want close 1008", err)
			}
		})
	}
}
