package stress

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Report is the outcome of a load run: counts + a latency distribution
// computed from the measured (post-warmup) requests.
type Report struct {
	Label       string
	Mode        string
	Concurrency int

	Requests   int           // measured (post-warmup) requests
	Elapsed    time.Duration // measured window wall-clock
	Throughput float64       // req/s over the measured window

	Success int
	ConnErr int // status < 0 (transport/conn error)
	HTTP4xx int
	HTTP5xx int

	P50, P95, P99, Max time.Duration
}

// Errors is the total non-success count.
func (r *Report) Errors() int { return r.ConnErr + r.HTTP4xx + r.HTTP5xx }

// ErrorRate is the error fraction over measured requests (0 when none).
func (r *Report) ErrorRate() float64 {
	if r.Requests == 0 {
		return 0
	}
	return float64(r.Errors()) / float64(r.Requests)
}

// newReport builds a Report from merged latency samples (nanoseconds) and
// status counts over the measured window.
func newReport(label, mode string, concurrency int, lat []int64, success, conn, c4, c5 int, elapsed time.Duration) *Report {
	r := &Report{
		Label: label, Mode: mode, Concurrency: concurrency,
		Requests: len(lat), Elapsed: elapsed,
		Success: success, ConnErr: conn, HTTP4xx: c4, HTTP5xx: c5,
	}
	if elapsed > 0 {
		r.Throughput = float64(len(lat)) / elapsed.Seconds()
	}
	if n := len(lat); n > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		r.P50 = time.Duration(lat[pctIdx(n, 50)])
		r.P95 = time.Duration(lat[pctIdx(n, 95)])
		r.P99 = time.Duration(lat[pctIdx(n, 99)])
		r.Max = time.Duration(lat[n-1])
	}
	return r
}

// pctIdx is the index into an n-element sorted slice for percentile p.
func pctIdx(n, p int) int {
	i := n * p / 100
	if i >= n {
		i = n - 1
	}
	return i
}

// String renders the report as a human block (the protobridge bench
// shape): counts, throughput, then the latency distribution.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- stress: %s (%s, concurrency %d) ---\n", r.Label, r.Mode, r.Concurrency)
	fmt.Fprintf(&b, "requests:    %d (measured)\n", r.Requests)
	fmt.Fprintf(&b, "duration:    %s\n", r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "throughput:  %.0f req/s\n", r.Throughput)
	fmt.Fprintf(&b, "success:     %d / %d (%.1f%%)\n", r.Success, r.Requests, successPct(r))
	if r.Errors() > 0 {
		fmt.Fprintf(&b, "errors:      %d (conn: %d, 4xx: %d, 5xx: %d)\n", r.Errors(), r.ConnErr, r.HTTP4xx, r.HTTP5xx)
	}
	if r.Requests > 0 {
		fmt.Fprintf(&b, "latency p50: %s\n", r.P50.Round(time.Microsecond))
		fmt.Fprintf(&b, "latency p95: %s\n", r.P95.Round(time.Microsecond))
		fmt.Fprintf(&b, "latency p99: %s\n", r.P99.Round(time.Microsecond))
		fmt.Fprintf(&b, "latency max: %s\n", r.Max.Round(time.Microsecond))
	}
	return b.String()
}

func successPct(r *Report) float64 {
	if r.Requests == 0 {
		return 0
	}
	return float64(r.Success) / float64(r.Requests) * 100
}

// Check evaluates the plan's thresholds against the report and returns a
// non-nil error listing every breached dimension (so a single run reports
// all failures at once). Returns nil when t is nil (report-only) or all
// gates pass.
func (r *Report) Check(t *Thresholds) error {
	if t == nil {
		return nil
	}
	var breaches []string
	gate := func(name string, limit, got time.Duration) {
		if limit > 0 && got > limit {
			breaches = append(breaches, fmt.Sprintf("%s %s > %s", name, got.Round(time.Microsecond), limit))
		}
	}
	gate("p50", t.P50, r.P50)
	gate("p95", t.P95, r.P95)
	gate("p99", t.P99, r.P99)
	gate("max", t.Max, r.Max)
	if t.ErrorRate > 0 && r.ErrorRate() > t.ErrorRate {
		breaches = append(breaches, fmt.Sprintf("error_rate %.4f > %.4f", r.ErrorRate(), t.ErrorRate))
	}
	if t.MinThroughput > 0 && r.Throughput < t.MinThroughput {
		breaches = append(breaches, fmt.Sprintf("throughput %.0f < %.0f req/s", r.Throughput, t.MinThroughput))
	}
	if len(breaches) == 0 {
		return nil
	}
	return fmt.Errorf("stress thresholds breached: %s", strings.Join(breaches, "; "))
}
