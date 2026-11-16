package stress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// A redirect is not a served call: API routes never answer 3xx, so one is a
// misrouted request (a proxy's login page, a trailing-slash redirect). It
// used to count as a success, because everything under 400 did.
func TestRun_A3xxIsNotASuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 2, 5*time.Second)
	plan := Plan{Concurrency: 2, TotalRequests: 20, Mode: ModePool, Selection: SelectRoundRobin, Timeout: 5 * time.Second}
	rep, err := Run(context.Background(), "redirect", plan, nil, []Op{workOp(false)}, nil, map[string]runner.Caller{}, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Success != 0 || rep.HTTP3xx != 20 || rep.ErrorRate() != 1 {
		t.Errorf("success=%d 3xx=%d error_rate=%v, want every redirect counted as an error", rep.Success, rep.HTTP3xx, rep.ErrorRate())
	}
}

// A preset without thresholds is report-only, which used to mean it could
// not fail: a run whose every request was refused exited 0. Report-only
// measures; it does not certify a run that measured nothing, or nothing
// that succeeded.
func TestCheck_ReportOnlyStillFailsARunWithNoSuccess(t *testing.T) {
	lat := []int64{1, 2, 3, 4}
	allRefused := newReport("x", ModePool, 1, lat, 0, 0, 4, 0, time.Second)
	if err := allRefused.Check(nil); err == nil || !strings.Contains(err.Error(), "no request succeeded") {
		t.Errorf("report-only over 100%% 4xx: err = %v, want a failure", err)
	}
	none := newReport("x", ModePool, 1, nil, 0, 0, 0, 0, time.Second)
	if err := none.Check(nil); err == nil || !strings.Contains(err.Error(), "no requests were measured") {
		t.Errorf("report-only over nothing measured: err = %v, want a failure", err)
	}
	partial := newReport("x", ModePool, 1, lat, 3, 0, 0, 1, time.Second)
	if err := partial.Check(nil); err != nil {
		t.Errorf("report-only with successes must stay report-only: %v", err)
	}
}
