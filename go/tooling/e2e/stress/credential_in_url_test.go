package stress

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// The load phase routes like the runner and carries the same credentials: a
// capability-link endpoint gets no bearer (its credential is in the URL),
// while an ordinary auth-required endpoint still does.
func TestFireOp_CapabilityLinkGetsNoBearer(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lc := NewLoadCaller(srv.URL, 1, 5*time.Second)
	ws := runtime.NewRun().NewScope()
	ops := []Op{
		{Endpoint: runner.Endpoint{Ref: "share.Open", Transport: "rest", HTTPMethod: "GET",
			PathTemplate: "/s/link", AuthRequired: true, CredentialInURL: true}},
		{Endpoint: runner.Endpoint{Ref: "tasks.List", Transport: "rest", HTTPMethod: "GET",
			PathTemplate: "/tasks", AuthRequired: true}},
	}
	for i := range ops {
		fireOp(ws, &ops[i], "session-token", nil, lc, &workerBuf{}, time.Time{})
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen["/s/link"]; got != "" {
		t.Errorf("capability-link endpoint received Authorization %q, want none", got)
	}
	if got := seen["/tasks"]; got != "Bearer session-token" {
		t.Errorf("auth-required endpoint received Authorization %q, want the bearer", got)
	}
}
