package restgw

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

func publicReq(remote string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = remote
	return r
}

// serve runs one gated unary call that answers status.
func serve(l *ratelimit.Limiter, remote string, status int) int {
	w := httptest.NewRecorder()
	rw, done, ok := AdmitPublic(l, w, publicReq(remote))
	if ok {
		rw.WriteHeader(status)
		done()
	}
	return w.Code
}

func TestAdmitPublic_ChargesFailuresPerClient(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 6, Burst: 2})
	for i := 0; i < 20; i++ {
		if got := serve(l, "203.0.113.9:1", http.StatusOK); got != http.StatusOK {
			t.Fatalf("successful call %d refused with %d — arrivals are being charged", i, got)
		}
	}
	for i := 0; i < 2; i++ {
		serve(l, "203.0.113.9:1", http.StatusUnauthorized)
	}
	w := httptest.NewRecorder()
	if _, _, ok := AdmitPublic(l, w, publicReq("203.0.113.9:1")); ok {
		t.Fatal("a drained bucket admitted the caller")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("refusal = %d, Retry-After %q; want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	if got := serve(l, "198.51.100.1:1", http.StatusOK); got != http.StatusOK {
		t.Errorf("another client was refused (%d) by the first one's failures", got)
	}
}

// A handler that writes a body without WriteHeader answered 200 and must not
// be charged; the recorder must still reach the server's writer.
func TestAdmitPublic_ImplicitOKAndUnwrap(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 6, Burst: 1})
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		rw, done, ok := AdmitPublic(l, w, publicReq("203.0.113.9:1"))
		if !ok {
			t.Fatalf("call %d refused: an implicit 200 was charged", i)
		}
		_, _ = rw.Write([]byte("{}"))
		done()
		if u, ok := rw.(interface{ Unwrap() http.ResponseWriter }); !ok || u.Unwrap() != w {
			t.Fatal("recorder does not unwrap to the server's writer")
		}
	}
}

func TestAdmitPublicStream_ChargesEveryStream(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 6, Burst: 2})
	for i := 0; i < 2; i++ {
		if !AdmitPublicStream(l, httptest.NewRecorder(), publicReq("203.0.113.9:1")) {
			t.Fatalf("stream %d refused inside the burst", i)
		}
	}
	w := httptest.NewRecorder()
	if AdmitPublicStream(l, w, publicReq("203.0.113.9:1")) || w.Code != http.StatusTooManyRequests {
		t.Errorf("third stream admitted (code %d); want 429", w.Code)
	}
	if !AdmitPublicStream(nil, httptest.NewRecorder(), publicReq("203.0.113.9:1")) {
		t.Error("nil limiter must admit")
	}
}

// Requests in flight together must each hold a token before the handler
// runs. Charging only after a failure was written admitted every concurrent
// request, so a caller's guess budget grew with its concurrency.
func TestAdmitPublic_ConcurrentFailuresStayWithinTheBurst(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 5})
	release := make(chan struct{})
	var (
		mu       sync.Mutex
		admitted int
		wg       sync.WaitGroup
	)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rw, done, ok := AdmitPublic(l, httptest.NewRecorder(), publicReq("203.0.113.9:1"))
			if !ok {
				return
			}
			defer done()
			mu.Lock()
			admitted++
			mu.Unlock()
			<-release // every admitted handler is still running
			rw.WriteHeader(http.StatusUnauthorized)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := admitted
		mu.Unlock()
		if n >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let any over-admission show
	close(release)
	wg.Wait()
	if admitted != 5 {
		t.Fatalf("%d of 100 concurrent failing requests reached the handler; the burst is 5", admitted)
	}
	if got := serve(l, "203.0.113.9:1", http.StatusOK); got != http.StatusTooManyRequests {
		t.Errorf("after the burst failed, the next request got %d, want 429", got)
	}
}

// A success gives its token back, so a burst of successes held open at once
// still leaves the bucket full.
func TestAdmitPublic_SuccessReturnsItsToken(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 2})
	var dones []func()
	for i := 0; i < 2; i++ {
		rw, done, ok := AdmitPublic(l, httptest.NewRecorder(), publicReq("203.0.113.9:1"))
		if !ok {
			t.Fatalf("request %d refused inside the burst", i)
		}
		rw.WriteHeader(http.StatusOK)
		dones = append(dones, done)
	}
	if _, _, ok := AdmitPublic(l, httptest.NewRecorder(), publicReq("203.0.113.9:1")); ok {
		t.Fatal("a third request was admitted while two held the burst")
	}
	for _, d := range dones {
		d()
	}
	for i := 0; i < 10; i++ {
		if got := serve(l, "203.0.113.9:1", http.StatusOK); got != http.StatusOK {
			t.Fatalf("success %d refused with %d after earlier successes returned their tokens", i, got)
		}
	}
}

// A handler that wrote nothing (it panicked before answering) did not
// succeed, and keeps its token.
func TestAdmitPublic_NothingWrittenIsCharged(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 1})
	_, done, ok := AdmitPublic(l, httptest.NewRecorder(), publicReq("203.0.113.9:1"))
	if !ok {
		t.Fatal("first request refused")
	}
	done()
	if _, _, ok := AdmitPublic(l, httptest.NewRecorder(), publicReq("203.0.113.9:1")); ok {
		t.Fatal("a request that wrote no response was refunded")
	}
}
