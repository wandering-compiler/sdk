package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The CORS allowlist governs fetches, not WebSocket upgrades: behind the
// CORS middleware, a page on an allowlisted foreign origin still cannot open
// a WebSocket — the upgrade follows the same-origin rule — and a page on the
// gateway's own origin can.
func TestAcceptWebSocket_SameOriginRuleIgnoresTheCORSAllowlist(t *testing.T) {
	const foreign = "https://app.example"
	h := restgw.CORSMiddleware(restgw.CORSConfig{AllowedOrigins: []string{foreign}, AllowCredentials: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := restgw.AcceptWebSocket(w, r)
			if err != nil {
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	dial := func(origin string) (*http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{origin}},
		})
		if conn != nil {
			_ = conn.CloseNow()
		}
		return resp, err
	}

	if resp, err := dial(foreign); err == nil {
		t.Fatal("an allowlisted foreign origin opened a WebSocket")
	} else if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin answered %d, want 403", resp.StatusCode)
	}
	if _, err := dial(srv.URL); err != nil {
		t.Fatalf("the gateway's own origin could not open a WebSocket: %v", err)
	}
}
