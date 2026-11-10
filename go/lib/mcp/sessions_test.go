package mcp

import (
	"context"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// syncMapLen counts the entries of the sync.Map field name of v (a pointer
// to a struct): the transport's and the MCP server's own session tables.
func syncMapLen(t *testing.T, v any, name string) int {
	t.Helper()
	f := reflect.ValueOf(v).Elem().FieldByName(name)
	if !f.IsValid() {
		t.Fatalf("%T has no field %s", v, name)
	}
	m := (*sync.Map)(unsafe.Pointer(f.UnsafeAddr()))
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// serveHTTP starts s on a free loopback port and waits until it answers.
func serveHTTP(t *testing.T, s *Server) string {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.ServeStreamableHTTP(ctx, addr); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 100; i++ {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			_ = resp.Body.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("transport never came up")
	return ""
}

func mcpPost(t *testing.T, addr, session, body, forwardedFor string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+EndpointPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func initialize(t *testing.T, addr, forwardedFor string) (int, string) {
	t.Helper()
	resp := mcpPost(t, addr, "", initializeBody, forwardedFor)
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

// A session no request touches for the idle TTL MUST be ended with all of
// its transport state: a client that initializes and never sends DELETE —
// crashed, reconnecting or hostile — otherwise holds ~10 KiB until restart,
// and a stream of initialize POSTs grows the process without bound. A session
// still in use is kept.
func TestServeStreamableHTTP_IdleSessionIsEnded(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetSessionLimits(SessionLimits{IdleTTL: 300 * time.Millisecond})
	addr := serveHTTP(t, s)

	for i := 0; i < 20; i++ {
		if code, id := initialize(t, addr, ""); code != http.StatusOK || id == "" {
			t.Fatalf("initialize %d: %d session=%q", i, code, id)
		}
	}
	_, kept := initialize(t, addr, "")
	if n := syncMapLen(t, s.inner, "sessions"); n != 21 {
		t.Fatalf("%d sessions registered, want 21", n)
	}
	// Keep one in use while the others go idle.
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if resp := mcpPost(t, addr, kept, ping, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("ping on the kept session: %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := syncMapLen(t, s.inner, "sessions"); n != 1 {
		t.Fatalf("%d sessions still registered after the idle TTL, want only the one in use", n)
	}
	if n := syncMapLen(t, s.transport.Load(), "activeSessions"); n != 1 {
		t.Fatalf("the transport still holds %d sessions' state, want 1", n)
	}
	if n := s.sessions.count(); n != 1 {
		t.Fatalf("the guard counts %d sessions, want 1", n)
	}
}

// A session with an open notification stream is in use however long the
// stream is quiet; it goes idle only once the stream ends.
func TestServeStreamableHTTP_OpenStreamKeepsItsSession(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetSessionLimits(SessionLimits{IdleTTL: 200 * time.Millisecond})
	addr := serveHTTP(t, s)
	_, id := initialize(t, addr, "")

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+EndpointPath, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", err, resp)
	}
	time.Sleep(700 * time.Millisecond)
	if n := syncMapLen(t, s.inner, "sessions"); n != 1 {
		t.Fatalf("a session with an open stream was ended (%d left)", n)
	}
	cancel()
	_ = resp.Body.Close()
	time.Sleep(700 * time.Millisecond)
	if n := s.sessions.count(); n != 0 {
		t.Fatalf("the session outlived its stream by the idle TTL (%d counted)", n)
	}
	if n := syncMapLen(t, s.inner, "sessions"); n != 0 {
		t.Fatalf("%d sessions registered after the stream ended and the TTL passed", n)
	}
}

// One client address MUST NOT hold more than its share of sessions, nor the
// process more than its cap: past them an initialize is refused (429 per
// address, 503 in total) before mcp-go keeps anything, and an ended session
// frees its slot.
func TestServeStreamableHTTP_SessionsAreCapped(t *testing.T) {
	s := NewServer("t", "1", nil)
	s.SetTrustedProxies([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	s.SetSessionLimits(SessionLimits{Max: 5, MaxPerAddress: 3})
	addr := serveHTTP(t, s)

	var a []string
	for i := 0; i < 3; i++ {
		code, id := initialize(t, addr, "203.0.113.1")
		if code != http.StatusOK {
			t.Fatalf("A's session %d: %d", i, code)
		}
		a = append(a, id)
	}
	resp := mcpPost(t, addr, "", initializeBody, "203.0.113.1")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("A's fourth session: %d Retry-After=%q, want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	for i := 0; i < 2; i++ {
		if code, _ := initialize(t, addr, "203.0.113.2"); code != http.StatusOK {
			t.Fatalf("B's session %d: %d", i, code)
		}
	}
	resp = mcpPost(t, addr, "", initializeBody, "203.0.113.3")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a sixth session in the process: %d, want 503 with Retry-After", resp.StatusCode)
	}
	if n := syncMapLen(t, s.inner, "sessions"); n != 5 {
		t.Fatalf("%d sessions registered, want the cap 5", n)
	}
	// A request on a live session still passes at the cap.
	if resp := mcpPost(t, addr, a[0], `{"jsonrpc":"2.0","id":2,"method":"ping"}`, "203.0.113.1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a request on a live session at the cap: %d", resp.StatusCode)
	}
	// DELETE frees a slot.
	req, _ := http.NewRequest(http.MethodDelete, "http://"+addr+EndpointPath, nil)
	req.Header.Set("Mcp-Session-Id", a[0])
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %v %v", err, r)
	}
	if code, _ := initialize(t, addr, "203.0.113.3"); code != http.StatusOK {
		t.Fatalf("after a DELETE: %d, want the freed slot", code)
	}
}

// The bounds read their environment overrides; a value that is not a
// positive whole number keeps the default — a bound cannot be switched off.
func TestSessionLimits_EnvAndDefaults(t *testing.T) {
	s := NewServer("t", "1", nil)
	if l := s.resolvedSessionLimits(); l != (SessionLimits{DefaultSessionIdleTTL, DefaultMaxSessions, DefaultMaxSessionsPerAddress}) {
		t.Fatalf("defaults: %+v", l)
	}
	t.Setenv(SessionIdleSecondsEnv, "60")
	t.Setenv(MaxSessionsEnv, "7")
	t.Setenv(MaxSessionsPerAddressEnv, "0")
	if l := s.resolvedSessionLimits(); l != (SessionLimits{time.Minute, 7, DefaultMaxSessionsPerAddress}) {
		t.Fatalf("env: %+v", l)
	}
	s.SetSessionLimits(SessionLimits{Max: 9})
	if l := s.resolvedSessionLimits(); l.Max != 9 {
		t.Fatalf("code beats env: %+v", l)
	}
}
