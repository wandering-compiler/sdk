package restgw

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// A public method that panics behind the panic middleware answers a 500
// envelope, reaches the operator as a report, and is still charged to the
// caller: a guess that crashes the handler is a guess.
func TestPublicMethodPanic_Is500ReportedAndCharged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	l := ratelimit.New(ratelimit.Config{PerMinute: 0.001, Burst: 1})
	h := RecoverPanicMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, done, ok := AdmitPublic(l, w, r)
		if !ok {
			return
		}
		defer done()
		_ = w
		panic("login handler exploded")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, publicReq("203.0.113.9:1"))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"code":"INTERNAL"`) {
		t.Fatalf("panic answered %d %s, want the 500 envelope", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "exploded") {
		t.Errorf("the panic value reached the client: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "login handler exploded") {
		t.Errorf("the panic was not reported; log: %q", buf.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, publicReq("203.0.113.9:1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the request after a panicking one answered %d, want 429 (the panic was charged)", rec.Code)
	}
}
