package restgw_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestBodyReadDeadline: the REST listener bounded only the header phase
// (ReadHeaderTimeout), and the body decode caps BYTES, not time — a client
// trickling a JSON body one byte a second held a goroutine and a
// connection for as long as it liked. BodyReadDeadline bounds the body
// read of a request that has one; a streaming request (an SSE GET, a
// WebSocket upgrade) has none and is untouched.
func TestBodyReadDeadline(t *testing.T) {
	readTook := make(chan time.Duration, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, _ = io.ReadAll(r.Body)
		readTook <- time.Since(start)
	})
	srv := httptest.NewServer(restgw.BodyReadDeadline(300 * time.Millisecond)(h))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 40\r\n\r\n{")
	go func() {
		for i := 0; i < 39; i++ {
			time.Sleep(100 * time.Millisecond)
			if _, err := io.WriteString(conn, " "); err != nil {
				return
			}
		}
	}()
	select {
	case took := <-readTook:
		if took > 2*time.Second {
			t.Fatalf("the trickled body was read for %v; the deadline did not bound it", took)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the handler was still reading a trickled body after 6s")
	}
}

// TestBodyReadDeadline_HandlerOutlivesIt: the deadline is the BODY's. Once
// the body is read, a handler that keeps working past it must keep its
// request context — net/http's background read after the body would
// otherwise hit the expired deadline and cancel the context.
func TestBodyReadDeadline_HandlerOutlivesIt(t *testing.T) {
	ctxErr := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(600 * time.Millisecond)
		ctxErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(restgw.BodyReadDeadline(200 * time.Millisecond)(h))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-ctxErr; err != nil {
		t.Fatalf("the request context was cancelled after the body was read: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
