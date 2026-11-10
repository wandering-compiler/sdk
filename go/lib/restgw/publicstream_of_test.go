package restgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// AdmitPublicStreamOf with its two limiters: the transport's stream cap
// (streams) and the method's own budget (l). A client at the cap is refused
// without spending l's token; a stream l refuses gives its cap slot back;
// an admitted stream holds its slot until its request ends.
func TestAdmitPublicStreamOf_CapOnStreamsTokenOnTheMethod(t *testing.T) {
	open := func(streams, l *ratelimit.Limiter) (bool, int, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest(http.MethodGet, "/live", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		ok := AdmitPublicStreamOf(streams, l, w, r)
		return ok, w.Code, cancel
	}

	t.Run("the cap spends no token", func(t *testing.T) {
		streams := ratelimit.New(ratelimit.Config{MaxStreams: 1})
		l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 2})
		ok, _, end1 := open(streams, l)
		if !ok {
			t.Fatal("first stream refused")
		}
		if ok, code, end := open(streams, l); ok || code != http.StatusTooManyRequests {
			end()
			t.Fatalf("second stream at the cap: admitted=%v code=%d, want 429", ok, code)
		}
		end1()
		// The slot frees once the first request's context has ended.
		deadline := time.Now().Add(2 * time.Second)
		for {
			ok, _, end := open(streams, l)
			if ok {
				end()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("after the first stream ended a new one is still refused: the cap refusal spent the method's token, or the slot was never freed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	t.Run("a method refusal gives the slot back", func(t *testing.T) {
		streams := ratelimit.New(ratelimit.Config{MaxStreams: 2})
		l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 1})
		ok, _, end1 := open(streams, l)
		defer end1()
		if !ok {
			t.Fatal("first stream refused")
		}
		if ok, code, end := open(streams, l); ok || code != http.StatusTooManyRequests {
			end()
			t.Fatalf("method budget spent: admitted=%v code=%d, want 429", ok, code)
		}
		// One stream is open; the refused one must not hold the second slot.
		r := httptest.NewRequest(http.MethodGet, "/live", nil)
		admitted, release := streams.OpenStream(r)
		if !admitted {
			t.Fatal("the stream the method refused still holds its cap slot")
		}
		release()
	})

	t.Run("both nil admit", func(t *testing.T) {
		if ok, _, end := open(nil, nil); !ok {
			t.Fatal("nil limiters refused a stream")
		} else {
			end()
		}
	})
}
