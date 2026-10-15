package restgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A WebSocket upgrade is sent cross-origin without CORS, and the library
// refuses every Origin but the gateway's own: a frontend the CORS allowlist
// admits for REST could not open its sockets. The allowlist now admits it on
// the socket too — except the `*` wildcard, which would let any site open a
// socket carrying its visitor's cookies.
func TestAcceptWebSocket_HonoursTheCORSAllowlist(t *testing.T) {
	accept := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	for _, tc := range []struct {
		name    string
		origins []string // nil = no CORS middleware at all
		origin  string
		ok      bool
	}{
		{"listed origin", []string{"https://app.example.com", "https://other.example.com"}, "https://app.example.com", true},
		{"origin not listed", []string{"https://app.example.com"}, "https://evil.example.com", false},
		{"same scheme+host only", []string{"https://app.example.com"}, "http://app.example.com", false},
		{"wildcard opens no socket", []string{"*"}, "https://evil.example.com", false},
		{"glob characters are literal", []string{"https://*.example.com"}, "https://evil.example.com", false},
		{"no allowlist: cross-origin refused", nil, "https://app.example.com", false},
		{"no Origin (not a browser)", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h http.Handler = accept
			if tc.origins != nil {
				h = CORSMiddleware(CORSConfig{AllowedOrigins: tc.origins}, accept)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hdr := http.Header{}
			if tc.origin != "" {
				hdr.Set("Origin", tc.origin)
			}
			conn, resp, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], &websocket.DialOptions{HTTPHeader: hdr})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if conn != nil {
				_ = conn.CloseNow()
			}
			if (err == nil) != tc.ok {
				t.Fatalf("origin %q with allowlist %v: dial error %v, want accepted=%v", tc.origin, tc.origins, err, tc.ok)
			}
		})
	}
}

// The same-origin case keeps working behind the middleware.
func TestAcceptWebSocket_SameOriginBehindCORS(t *testing.T) {
	srv := httptest.NewServer(CORSMiddleware(CORSConfig{AllowedOrigins: []string{"https://app.example.com"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if conn, err := AcceptWebSocket(w, r); err == nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}
		})))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("same-origin socket refused: %v", err)
	}
	_ = conn.CloseNow()
}
