package lifecycle_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
)

// serve starts srv on a loopback listener and returns its base URL.
func serve(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func get(url string) error {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// The per-listener base contexts are released by a graceful stop too —
// bounded or not — not only by the forced close.
func TestStopHTTP_GracefulStopCancelsBaseContexts(t *testing.T) {
	for name, timeout := range map[string]time.Duration{"bounded": 10 * time.Second, "unbounded": 0} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var base context.Context
			srv := &http.Server{
				Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
				ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
					mu.Lock()
					base = ctx
					mu.Unlock()
					return ctx
				},
			}
			stopper := lifecycle.BindHTTP(srv)
			if err := get(serve(t, srv)); err != nil {
				t.Fatal(err)
			}
			if err := stopper.Stop(timeout); err != nil {
				t.Fatalf("Stop = %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if base == nil {
				t.Fatal("no connection context captured")
			}
			if base.Err() == nil {
				t.Error("the listener's base context is still live after a graceful stop")
			}
		})
	}
}

// Repeated Shutdown calls and a forced close each reach the drain
// broadcast; the channel is closed once and nothing panics.
func TestStopHTTP_DrainClosedOnceAcrossRepeatedStops(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // ignores the drain: forces the close path
	})}
	stopper := lifecycle.BindHTTP(srv)
	url := serve(t, srv)
	conn := holdSSE(t, url[len("http://"):])
	defer func() { _ = conn.Close() }()

	// Forced: Shutdown's hook and forceClose both start the drain.
	if err := stopper.Stop(200 * time.Millisecond); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	// The bound server shut down twice more, as further callers would;
	// each Shutdown runs the hook again.
	for i := 0; i < 2; i++ {
		_ = srv.Shutdown(context.Background())
	}
	// The hooks run on their own goroutines; give a second close the time
	// to panic the test binary.
	time.Sleep(200 * time.Millisecond)
}

type ctxKey struct{}

// A BaseContext the caller already set stays the parent of every request
// context: its values reach handlers, alongside the drain signal.
func TestBindHTTP_ComposesAnExistingBaseContext(t *testing.T) {
	got := make(chan context.Context, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got <- r.Context() }),
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(context.Background(), ctxKey{}, "tenant-pool")
		},
	}
	stopper := lifecycle.BindHTTP(srv)
	defer func() { _ = stopper.Stop(time.Second) }()
	if err := get(serve(t, srv)); err != nil {
		t.Fatal(err)
	}
	ctx := <-got
	if v, _ := ctx.Value(ctxKey{}).(string); v != "tenant-pool" {
		t.Errorf("request context value = %q, want the caller's BaseContext value", v)
	}
	if lifecycle.Draining(ctx) == nil {
		t.Error("request context carries no drain signal")
	}
}

// A shutdown that fails for a reason other than its deadline is returned
// as the error, and the server is not force-closed over it.
func TestStopHTTP_NonDeadlineErrorIsReturnedWithoutForceClose(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	stopper := lifecycle.BindHTTP(srv)
	url := serve(t, srv)
	boom := errors.New("session sweeper: store unreachable")

	if err := stopper.StopWith(5*time.Second, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("StopWith = %v, want the shutdown's own error", err)
	}
	if err := get(url); err != nil {
		t.Errorf("the server was force-closed after a non-deadline error: %v", err)
	}
}
