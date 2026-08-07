package grpcclient_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// serviceConfig mirrors the parts of the gRPC service-config JSON we make
// guarantees about. A typo in DefaultServiceConfig is otherwise SILENT —
// gRPC discards an unparseable service config at dial time and falls back to
// NO retry policy, so a broken brace would disable transparent UNAVAILABLE
// replay in production without any test or startup error firing. Pinning the
// parsed shape turns that into a unit failure.
type serviceConfig struct {
	MethodConfig []struct {
		Name        []map[string]any `json:"name"`
		RetryPolicy *struct {
			MaxAttempts          int      `json:"maxAttempts"`
			InitialBackoff       string   `json:"initialBackoff"`
			MaxBackoff           string   `json:"maxBackoff"`
			BackoffMultiplier    float64  `json:"backoffMultiplier"`
			RetryableStatusCodes []string `json:"retryableStatusCodes"`
		} `json:"retryPolicy"`
	} `json:"methodConfig"`
	RetryThrottling *struct {
		MaxTokens  int     `json:"maxTokens"`
		TokenRatio float64 `json:"tokenRatio"`
	} `json:"retryThrottling"`
}

// TestDefaultServiceConfig_IsValidJSON guards the most basic regression: the
// embedded config must parse. A trailing comma / unbalanced brace here ships
// a retry-less channel silently.
func TestDefaultServiceConfig_IsValidJSON(t *testing.T) {
	var sc serviceConfig
	if err := json.Unmarshal([]byte(grpcclient.DefaultServiceConfig), &sc); err != nil {
		t.Fatalf("DefaultServiceConfig is not valid JSON: %v", err)
	}
	if len(sc.MethodConfig) != 1 || sc.MethodConfig[0].RetryPolicy == nil {
		t.Fatalf("expected exactly one methodConfig carrying a retryPolicy; got %+v", sc.MethodConfig)
	}
}

// TestDefaultServiceConfig_RetriesOnlyUnavailable is the SAFETY invariant: the
// policy may retry ONLY codes.UNAVAILABLE — the request provably never reached
// the server, so replaying it is safe even for non-idempotent mutations. Any
// code where the server might have already applied the write (ABORTED,
// DEADLINE_EXCEEDED, INTERNAL, …) must NOT be retryable, or a dropped-response
// mutation could be double-applied. This pins the exact set so a "let's also
// retry ABORTED" edit fails loudly.
func TestDefaultServiceConfig_RetriesOnlyUnavailable(t *testing.T) {
	var sc serviceConfig
	if err := json.Unmarshal([]byte(grpcclient.DefaultServiceConfig), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	codes := sc.MethodConfig[0].RetryPolicy.RetryableStatusCodes
	if len(codes) != 1 || codes[0] != "UNAVAILABLE" {
		t.Errorf("retryableStatusCodes = %v, want exactly [UNAVAILABLE] (replaying any other code risks double-applying a mutation)", codes)
	}
	if got := sc.MethodConfig[0].RetryPolicy.MaxAttempts; got != 3 {
		t.Errorf("maxAttempts = %d, want 3", got)
	}
}

// TestDefaultServiceConfig_HasRetryThrottling pins the anti-retry-storm token
// bucket: without channel-level throttling a backend outage turns every client
// into a retry amplifier (3× the load on an already-failing server). The bucket
// must be present and configured so sustained failures suspend retries.
func TestDefaultServiceConfig_HasRetryThrottling(t *testing.T) {
	var sc serviceConfig
	if err := json.Unmarshal([]byte(grpcclient.DefaultServiceConfig), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc.RetryThrottling == nil {
		t.Fatal("retryThrottling missing — a backend outage can be amplified into a retry storm")
	}
	if sc.RetryThrottling.MaxTokens <= 0 || sc.RetryThrottling.TokenRatio <= 0 {
		t.Errorf("retryThrottling = %+v, want positive maxTokens + tokenRatio", *sc.RetryThrottling)
	}
}

// TestDefaultClientKeepalive_PairsWithServerEnforcement pins the keepalive
// params against the documented server-side EnforcementPolicy contract. The
// footgun: a client pinging FASTER than the server's MinTime (<=30s) is killed
// with GOAWAY "too_many_pings". So Time must stay >= 30s, both sides move
// together, and PermitWithoutStream must be true (idle service-to-service
// connections carry no active stream). Timeout must be a positive, sub-Time
// window so a half-dead peer is detected without false-positive teardowns.
func TestDefaultClientKeepalive_PairsWithServerEnforcement(t *testing.T) {
	kp := grpcclient.DefaultClientKeepalive
	if kp.Time < 30*time.Second {
		t.Errorf("keepalive Time = %v, want >= 30s (server EnforcementPolicy MinTime); a faster ping triggers GOAWAY too_many_pings", kp.Time)
	}
	if !kp.PermitWithoutStream {
		t.Error("PermitWithoutStream = false; idle service-to-service connections would never ping and get reaped by NAT/LB")
	}
	if kp.Timeout <= 0 || kp.Timeout >= kp.Time {
		t.Errorf("keepalive Timeout = %v, want 0 < Timeout < Time (%v)", kp.Timeout, kp.Time)
	}
}
