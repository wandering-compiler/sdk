package healthcheck_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/core/healthz"
	"github.com/wandering-compiler/sdk/go/service/healthcheck"
)

// A /healthz that redirects is not answered by the listener the probe
// asked, so the probe reports the redirect instead of following it to a
// healthy page.
func TestRun_HTTPRedirectIsNotFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(healthz.Path, http.RedirectHandler("/elsewhere", http.StatusFound))
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := listenHTTP(t, mux)

	out, err := run(t, nil, healthcheck.HTTP("rest", addr))
	if err == nil {
		t.Fatalf("a redirecting /healthz passed the probe\n%s", out)
	}
	if !strings.Contains(out, "302") {
		t.Errorf("output should name the redirect status:\n%s", out)
	}
}

// The probe reads at most 4 KiB of the body: a listener that answers 200
// and never ends the body does not hold the probe until its timeout.
func TestRun_HTTPBodyReadIsCapped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthz.Path, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat("x", 1024))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	addr := listenHTTP(t, mux)

	start := time.Now()
	out, err := run(t, nil, healthcheck.HTTP("rest", addr))
	if err != nil {
		t.Fatalf("err = %v, want healthy after the capped read\n%s", err, out)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("probe took %s; the body read was not capped", took)
	}
}
