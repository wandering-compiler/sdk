package stress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// testServer is a tiny gateway stand-in: /login issues a token, /work
// echoes 200 (and counts hits), /fail returns 500.
func testServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"T-123"}`))
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	return httptest.NewServer(mux)
}

func loginStep() runner.Step {
	return runner.Step{
		Endpoint: runner.Endpoint{Ref: "x.Auth.Login", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/login"},
		Input:    map[string]any{"email": "a@b.c"},
		Expect:   map[string]any{"token": map[string]any{"capture": "auth.token"}},
		Label:    "login",
	}
}

func workOp(authed bool) Op {
	return Op{
		Endpoint: runner.Endpoint{Ref: "x.Svc.Work", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/work", AuthRequired: authed},
		Input:    map[string]any{"n": "${worker}-${seq}"},
		Label:    "work",
	}
}

func TestRun_PoolTotalRequests(t *testing.T) {
	var hits atomic.Int64
	srv := testServer(t, &hits)
	defer srv.Close()

	callers := map[string]runner.Caller{"rest": runner.NewRESTCaller(srv.URL, nil)}
	lc := NewLoadCaller(srv.URL, 8, 5*time.Second)

	plan := Plan{Concurrency: 8, TotalRequests: 200, Mode: ModePool, Selection: SelectRoundRobin, Timeout: 5 * time.Second}
	rep, err := Run(context.Background(), "pool", plan, []runner.Step{loginStep()}, []Op{workOp(true)}, nil, callers, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Requests != 200 {
		t.Errorf("measured requests = %d, want 200", rep.Requests)
	}
	if got := hits.Load(); got != 200 {
		t.Errorf("server hits = %d, want 200", got)
	}
	if rep.Success != 200 || rep.Errors() != 0 {
		t.Errorf("success=%d errors=%d, want 200/0", rep.Success, rep.Errors())
	}
	if rep.Throughput <= 0 {
		t.Errorf("throughput = %v, want > 0", rep.Throughput)
	}
}

func TestRun_SequenceMode(t *testing.T) {
	var hits atomic.Int64
	srv := testServer(t, &hits)
	defer srv.Close()

	callers := map[string]runner.Caller{"rest": runner.NewRESTCaller(srv.URL, nil)}
	lc := NewLoadCaller(srv.URL, 4, 5*time.Second)

	// 2 ops per sequence iteration × 50 iterations = 100 fired requests.
	plan := Plan{Concurrency: 4, TotalRequests: 50, Mode: ModeSequence, Timeout: 5 * time.Second}
	rep, err := Run(context.Background(), "seq", plan, nil, []Op{workOp(false), workOp(false)}, nil, callers, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Requests != 100 {
		t.Errorf("measured requests = %d, want 100 (50 iters × 2 ops)", rep.Requests)
	}
	if hits.Load() != 100 {
		t.Errorf("server hits = %d, want 100", hits.Load())
	}
}

func TestRun_DurationMode(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 4, 5*time.Second)

	plan := Plan{Concurrency: 4, Duration: 300 * time.Millisecond, Mode: ModePool, Selection: SelectRandom, Timeout: 5 * time.Second}
	rep, err := Run(context.Background(), "dur", plan, nil, []Op{workOp(false)}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Requests == 0 {
		t.Errorf("duration run fired no measured requests")
	}
	if rep.Elapsed < 200*time.Millisecond {
		t.Errorf("measured elapsed = %v, want ~300ms", rep.Elapsed)
	}
}

func TestRun_ThresholdsBreachAndPass(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 4, 5*time.Second)

	failOp := Op{
		Endpoint: runner.Endpoint{Ref: "x.Svc.Fail", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/fail"},
		Label:    "fail",
	}
	plan := Plan{Concurrency: 4, TotalRequests: 40, Mode: ModePool, Timeout: 5 * time.Second}
	rep, err := Run(context.Background(), "thr", plan, nil, []Op{failOp}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.HTTP5xx != 40 {
		t.Errorf("5xx = %d, want 40", rep.HTTP5xx)
	}
	// error_rate 1.0 must breach a 0.01 gate.
	if err := rep.Check(&Thresholds{ErrorRate: 0.01}); err == nil {
		t.Errorf("expected error_rate breach, got nil")
	}
	// A generous gate passes.
	if err := rep.Check(&Thresholds{ErrorRate: 1.0}); err != nil {
		t.Errorf("expected pass, got %v", err)
	}
}
