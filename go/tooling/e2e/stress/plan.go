// Package stress is the load-test engine half of the generated e2erunner.
// It reuses the e2e routing table (runner.Endpoint), input interpolation
// (runtime.Expand) and once-only setup execution (runner.RunSteps), but
// drives load over a tuned fasthttp client (LoadCaller) so the runner is
// never the bottleneck. It is imported ONLY by the generated e2erunner
// test binary — never by the gateway — so the fasthttp dependency stays
// out of the shipped service binary.
//
// A stress preset is a directory: setup/ (run once — login, seed), load/
// (the ops driven under load) and teardown/ (run once). The Plan (from the
// preset's stress.yaml) governs how the load ops are hammered.
package stress

import (
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// Plan is the runtime (parsed) load configuration — the typed form of
// spec.StressPlan, baked into the e2erunner by the generator. Durations
// are resolved; a zero value of an optional knob means "off".
type Plan struct {
	Concurrency   int
	TotalRequests int           // 0 when Duration drives the run
	Duration      time.Duration // 0 when TotalRequests drives the run
	Mode          string        // "pool" | "sequence"
	Selection     string        // "round_robin" | "random" (pool only)
	Warmup        time.Duration
	Ramp          time.Duration
	Timeout       time.Duration
	ThinkTime     time.Duration
	Thresholds    *Thresholds
}

// Mode + selection values, mirroring spec.StressMode*/StressSelect*.
const (
	ModePool         = "pool"
	ModeSequence     = "sequence"
	SelectRoundRobin = "round_robin"
	SelectRandom     = "random"
)

// Thresholds is the optional pass/fail gate. A zero field is "no gate on
// this dimension".
type Thresholds struct {
	P50, P95, P99, Max time.Duration
	ErrorRate          float64
	MinThroughput      float64
}

// Op is one baked load call: routing + the request contract. Distinct
// from runner.Step (which the asserting e2e path uses) because the load
// engine fires ops via the fasthttp LoadCaller and measures status +
// latency rather than asserting a response body.
type Op struct {
	Endpoint runner.Endpoint
	Input    map[string]any
	Headers  map[string]string
	Label    string
}
