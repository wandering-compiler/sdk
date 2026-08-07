package stress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// --- report.go: String / successPct / ErrorRate / pctIdx / Check ------

func TestReportStringAndPercents(t *testing.T) {
	r := newReport("lbl", ModePool, 4,
		[]int64{1000, 2000, 3000, 4000}, 3, 1, 0, 0, 100*time.Millisecond)
	s := r.String()
	for _, want := range []string{"stress: lbl", "requests:", "throughput:", "success:", "errors:", "latency p50:"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q\n%s", want, s)
		}
	}
	if successPct(r) <= 0 {
		t.Errorf("successPct = %v, want > 0", successPct(r))
	}
	// zero-request report → percentages clamp to 0 (no divide-by-zero)
	empty := &Report{}
	if successPct(empty) != 0 || empty.ErrorRate() != 0 {
		t.Errorf("empty report pct = %v / %v, want 0/0", successPct(empty), empty.ErrorRate())
	}
	if empty.String() == "" {
		t.Error("empty report should still render a header block")
	}
}

func TestPctIdxClamp(t *testing.T) {
	// p=100 forces i==n → clamped to n-1
	if got := pctIdx(3, 100); got != 2 {
		t.Errorf("pctIdx(3,100) = %d, want 2 (clamped)", got)
	}
	if got := pctIdx(4, 50); got != 2 {
		t.Errorf("pctIdx(4,50) = %d, want 2", got)
	}
}

func TestCheckAllDimensions(t *testing.T) {
	r := &Report{
		Requests: 100, Throughput: 50,
		Success: 90, HTTP5xx: 10,
		P50: 10 * time.Millisecond, P95: 20 * time.Millisecond,
		P99: 30 * time.Millisecond, Max: 40 * time.Millisecond,
	}
	// nil thresholds → no gate
	if err := r.Check(nil); err != nil {
		t.Errorf("nil thresholds should pass: %v", err)
	}
	// every latency dimension + throughput breaches at once
	err := r.Check(&Thresholds{
		P50: time.Millisecond, P95: time.Millisecond, P99: time.Millisecond,
		Max: time.Millisecond, MinThroughput: 1000,
	})
	if err == nil {
		t.Fatal("tight gates should breach")
	}
	for _, dim := range []string{"p50", "p95", "p99", "max", "throughput"} {
		if !strings.Contains(err.Error(), dim) {
			t.Errorf("breach message missing %q: %v", dim, err)
		}
	}
	// generous gates pass
	if err := r.Check(&Thresholds{P50: time.Second, MinThroughput: 1}); err != nil {
		t.Errorf("generous gates should pass: %v", err)
	}
}

// --- caller.go: NewLoadCaller defaults + Fire error/header arms -------

func TestNewLoadCaller_Defaults(t *testing.T) {
	// timeout <= 0 → default 30s; concurrency*2 < 512 → floored to 512
	c := NewLoadCaller("http://h/", 8, 0)
	if c.timeout != 30*time.Second {
		t.Errorf("default timeout = %v, want 30s", c.timeout)
	}
	if c.BaseURL != "http://h" {
		t.Errorf("BaseURL = %q, want trimmed", c.BaseURL)
	}
	if c.client.MaxConnsPerHost != 512 {
		t.Errorf("MaxConnsPerHost = %d, want 512 floor", c.client.MaxConnsPerHost)
	}
	// high concurrency → maxConns = concurrency*2 (above the floor)
	big := NewLoadCaller("http://h", 400, 2*time.Second)
	if big.client.MaxConnsPerHost != 800 {
		t.Errorf("MaxConnsPerHost = %d, want 800", big.client.MaxConnsPerHost)
	}
}

func TestLoadCallerFire(t *testing.T) {
	var gotAuth, gotHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHdr = r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewLoadCaller(srv.URL, 4, 2*time.Second)
	ep := runner.Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/m"}
	code, err := c.Fire(ep, map[string]any{"a": 1}, "tok",
		map[string]string{"X-Trace": "abc", "Authorization": "should-be-skipped"})
	if err != nil || code != 200 {
		t.Fatalf("Fire = %d,%v; want 200,nil", code, err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok (static header must not override)", gotAuth)
	}
	if gotHdr != "abc" {
		t.Errorf("X-Trace = %q, want abc", gotHdr)
	}
}

func TestLoadCallerFire_ResolveError(t *testing.T) {
	c := NewLoadCaller("http://h", 4, time.Second)
	// path param declared but absent from input → ResolveREST error → -1
	ep := runner.Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/m/{id}", PathParams: []string{"id"}}
	if code, err := c.Fire(ep, map[string]any{}, "", nil); code != -1 || err == nil {
		t.Errorf("resolve error = %d,%v; want -1,err", code, err)
	}
}

func TestLoadCallerFire_TransportError(t *testing.T) {
	// nothing listening → DoTimeout transport error → -1
	c := NewLoadCaller("http://127.0.0.1:1", 4, 200*time.Millisecond)
	ep := runner.Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/m"}
	if code, err := c.Fire(ep, map[string]any{"a": 1}, "", nil); code != -1 || err == nil {
		t.Errorf("transport error = %d,%v; want -1,err", code, err)
	}
}

// --- engine.go: Run error arms, pickIdx, fireOp expand error, warmup --

func TestRun_NoLoadOps(t *testing.T) {
	if _, err := Run(context.Background(), "x", Plan{Concurrency: 1, TotalRequests: 1}, nil, nil, nil, nil, nil); err == nil {
		t.Error("no load ops should error")
	}
}

func TestRun_SetupFailureAborts(t *testing.T) {
	// a setup step pointing at a transport with no caller fails RunSteps.
	bad := runner.Step{Endpoint: runner.Endpoint{Ref: "x.A.B", Transport: "nope"}, Label: "setup"}
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 1, time.Second)
	_, err := Run(context.Background(), "x", Plan{Concurrency: 1, TotalRequests: 1, Mode: ModePool},
		[]runner.Step{bad}, []Op{workOp(false)}, nil, map[string]runner.Caller{}, lc)
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Errorf("setup failure should abort with setup error: %v", err)
	}
}

func TestRun_TeardownFailureNonFatal(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 2, 2*time.Second)
	callers := map[string]runner.Caller{"rest": runner.NewRESTCaller(srv.URL, nil)}
	// teardown step with an unresolved transport → logged, not fatal
	bad := runner.Step{Endpoint: runner.Endpoint{Ref: "x.A.B", Transport: "nope"}, Label: "teardown"}
	rep, err := Run(context.Background(), "x", Plan{Concurrency: 2, TotalRequests: 20, Mode: ModePool},
		nil, []Op{workOp(false)}, []runner.Step{bad}, callers, lc)
	if err != nil {
		t.Fatalf("teardown failure must not void the run: %v", err)
	}
	if rep.Requests != 20 {
		t.Errorf("requests = %d, want 20", rep.Requests)
	}
}

func TestRun_WarmupExcludesSamples(t *testing.T) {
	var hits atomic.Int64
	srv := testServer(t, &hits)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 2, 2*time.Second)
	// warmup discards early samples; duration mode so the window is real.
	plan := Plan{Concurrency: 2, Duration: 250 * time.Millisecond, Warmup: 60 * time.Millisecond,
		Mode: ModePool, Selection: SelectRoundRobin, Timeout: 2 * time.Second}
	rep, err := Run(context.Background(), "warmup", plan, nil, []Op{workOp(false)}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// fired more than measured (warmup samples discarded)
	if int64(rep.Requests) > hits.Load() {
		t.Errorf("measured %d > fired %d; warmup should discard", rep.Requests, hits.Load())
	}
}

func TestRun_PoolRandomSelection(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 4, 2*time.Second)
	// >1 op + random selection exercises pickIdx's random branch
	plan := Plan{Concurrency: 4, TotalRequests: 60, Mode: ModePool, Selection: SelectRandom, Timeout: 2 * time.Second}
	rep, err := Run(context.Background(), "rand", plan, nil, []Op{workOp(false), workOp(false), workOp(false)}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Requests != 60 {
		t.Errorf("requests = %d, want 60", rep.Requests)
	}
}

func TestRun_RampAndThinkTime(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 3, 2*time.Second)
	// Ramp staggers worker starts; ThinkTime sleeps between iterations.
	plan := Plan{Concurrency: 3, TotalRequests: 12, Mode: ModePool, Selection: SelectRoundRobin,
		Ramp: 30 * time.Millisecond, ThinkTime: 5 * time.Millisecond, Timeout: 2 * time.Second}
	rep, err := Run(context.Background(), "ramp", plan, nil, []Op{workOp(false), workOp(false)}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Requests != 12 {
		t.Errorf("requests = %d, want 12", rep.Requests)
	}
}

func TestFireOp_ExpandErrorCountedAsConn(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 1, time.Second)
	op := Op{
		Endpoint: runner.Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/work"},
		Input:    map[string]any{"a": "${nope}"}, // unresolvable → malformed op
		Label:    "bad",
	}
	plan := Plan{Concurrency: 1, TotalRequests: 5, Mode: ModePool, Timeout: time.Second}
	rep, err := Run(context.Background(), "expanderr", plan, nil, []Op{op}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// every fire failed to expand → counted as connection-class errors
	if rep.ConnErr == 0 {
		t.Errorf("expand errors should count as conn errors, got %d", rep.ConnErr)
	}
}

func TestFireOp_ConnAnd4xxStatuses(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()

	// 4xx: hit an unrouted path on the test mux → 404.
	lc := NewLoadCaller(srv.URL, 1, time.Second)
	op404 := Op{Endpoint: runner.Endpoint{Ref: "x.Svc.NF", Transport: "rest", HTTPMethod: "GET", PathTemplate: "/missing"}, Label: "nf"}
	rep, err := Run(context.Background(), "c4", Plan{Concurrency: 1, TotalRequests: 5, Mode: ModePool, Timeout: time.Second},
		nil, []Op{op404}, nil, nil, lc)
	if err != nil {
		t.Fatalf("Run 4xx: %v", err)
	}
	if rep.HTTP4xx == 0 {
		t.Errorf("404 path should count as 4xx, got %d", rep.HTTP4xx)
	}

	// conn error: load caller pointed at a dead address → status -1.
	dead := NewLoadCaller("http://127.0.0.1:1", 1, 200*time.Millisecond)
	opWork := Op{Endpoint: runner.Endpoint{Ref: "x.Svc.M", Transport: "rest", HTTPMethod: "POST", PathTemplate: "/work"}, Input: map[string]any{"a": 1}, Label: "w"}
	rep2, err := Run(context.Background(), "conn", Plan{Concurrency: 1, TotalRequests: 3, Mode: ModePool, Timeout: 200 * time.Millisecond},
		nil, []Op{opWork}, nil, nil, dead)
	if err != nil {
		t.Fatalf("Run conn: %v", err)
	}
	if rep2.ConnErr == 0 {
		t.Errorf("dead address should count as conn errors, got %d", rep2.ConnErr)
	}
}

func TestWorker_RampContextCancel(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 4, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: high-offset workers exit via the ramp ctx.Done arm
	plan := Plan{Concurrency: 4, TotalRequests: 1000, Mode: ModePool, Selection: SelectRoundRobin,
		Ramp: 10 * time.Second, Timeout: time.Second}
	if _, err := Run(ctx, "rampcancel", plan, nil, []Op{workOp(false)}, nil, nil, lc); err != nil {
		t.Fatalf("ramp-cancel run should report: %v", err)
	}
}

func TestWorker_ThinkTimeContextCancel(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	// cancel shortly after the worker enters its (long) think-time sleep
	timer := time.AfterFunc(40*time.Millisecond, cancel)
	defer timer.Stop()
	plan := Plan{Concurrency: 1, TotalRequests: 1000, Mode: ModePool,
		ThinkTime: 10 * time.Second, Timeout: time.Second}
	if _, err := Run(ctx, "thinkcancel", plan, nil, []Op{workOp(false)}, nil, nil, lc); err != nil {
		t.Fatalf("think-cancel run should report: %v", err)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	srv := testServer(t, nil)
	defer srv.Close()
	lc := NewLoadCaller(srv.URL, 2, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → stop predicate trips immediately
	plan := Plan{Concurrency: 2, TotalRequests: 1000, Mode: ModePool, Timeout: time.Second}
	if _, err := Run(ctx, "cancel", plan, nil, []Op{workOp(false)}, nil, nil, lc); err != nil {
		t.Fatalf("cancelled run should still report: %v", err)
	}
}
