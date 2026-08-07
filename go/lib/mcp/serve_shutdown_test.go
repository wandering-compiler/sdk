package mcp

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
)

// freeAddr reserves a loopback port and releases it, so the streamable
// transport (whose Start owns its listener) has a known address to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// A client parked on the MCP notification stream (the long-lived GET) must
// not hold the transport open through the whole graceful window: the
// streams end as shutdown starts, and ServeStreamableHTTP returns without
// an error, so the deploy is neither slow nor falsely red.
func TestServeStreamableHTTPDrainsWithOpenStream(t *testing.T) {
	addr := freeAddr(t)
	s := NewServer("t", "1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.ServeStreamableHTTP(ctx, addr) }()

	// Wait for the listener, then park on the notification stream.
	var conn net.Conn
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			conn = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		cancel()
		t.Fatal("transport never came up")
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET " + EndpointPath + " HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("stream did not open: %q", line)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ServeStreamableHTTP returned %v — an open stream is not a failed shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStreamableHTTP never returned")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("shutdown took %s with one idle MCP stream — waited out instead of drained", took)
	}
}

// The drain arm is scoped to the streaming GET: a POST (a tool call) keeps
// its request context, i.e. it keeps the full drain budget instead of
// being cut the instant shutdown starts.
func TestDrainAwareStreamLeavesNonGETContextAlone(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	cut := make(chan struct{}, 1)
	h := drainAwareStream(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			cut <- struct{}{}
		case <-release:
		}
	}))

	srv := &http.Server{Handler: h}
	stopper := lifecycle.BindHTTP(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	go func() {
		resp, perr := http.Post("http://"+ln.Addr().String()+EndpointPath, "application/json", nil)
		if perr == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	// Start the drain; the in-flight POST's context must survive it.
	go func() { _ = stopper.Stop(5 * time.Second) }()
	select {
	case <-cut:
		t.Fatal("an in-flight POST was cancelled by the drain signal — tool calls must keep their budget")
	case <-time.After(300 * time.Millisecond):
	}
}
