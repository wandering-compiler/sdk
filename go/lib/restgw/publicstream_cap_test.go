package restgw

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// streamServer serves a public SSE stream at /sse and a public WebSocket at
// /ws, both admitted by AdmitPublicStream on l and held until the client
// goes away.
func streamServer(t *testing.T, l *ratelimit.Limiter) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if !AdmitPublicStream(l, w, r) {
			return
		}
		flusher, _ := WriteSSEHeaders(w)
		_, _ = w.Write([]byte(": open\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !AdmitPublicStream(l, w, r) {
			return
		}
		conn, err := AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, _ = conn.Read(r.Context())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// openSSE opens a stream and returns its status and a close func.
func openSSE(t *testing.T, base string) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	}
	// Closed at the latest before the server is: its Close waits for the
	// handler, which waits for this client to go away.
	closeIt := func() { cancel(); _ = resp.Body.Close() }
	t.Cleanup(closeIt)
	return resp.StatusCode, closeIt
}

// With MaxStreams set, one client's open SSE and WebSocket streams count
// together against the cap; the next is refused with 429, and closing one
// frees its slot once its handler has returned.
func TestAdmitPublicStream_CapsOpenStreamsAndFreesAClosedOne(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 6000, Burst: 100, MaxStreams: 2})
	base := streamServer(t, l)

	code, closeSSE := openSSE(t, base)
	if code != http.StatusOK {
		t.Fatalf("first stream: %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+base[len("http"):]+"/ws", &websocket.DialOptions{Subprotocols: []string{wsSubprotocolPB}})
	if err != nil {
		t.Fatalf("second stream (WebSocket): %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })

	if code, c := openSSE(t, base); code != http.StatusTooManyRequests {
		c()
		t.Fatalf("third stream: %d, want 429 over a cap of 2", code)
	} else {
		c()
	}

	closeSSE()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, c := openSSE(t, base)
		c()
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after closing a stream the next one is still refused (%d)", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Without MaxStreams, the default, a client keeps as many streams open as
// its token budget lets it open.
func TestAdmitPublicStream_NoCapByDefault(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 6000, Burst: 100})
	base := streamServer(t, l)
	for i := range 20 {
		code, _ := openSSE(t, base)
		if code != http.StatusOK {
			t.Fatalf("stream %d: %d without a cap", i, code)
		}
	}
}
