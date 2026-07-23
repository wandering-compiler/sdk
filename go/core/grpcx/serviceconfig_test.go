package grpcx

import (
	"encoding/json"
	"testing"
	"time"
)

// These tests pin the SEMANTICS of the embedded retry/keepalive config, which
// the existing DialOpts coverage exercises but never asserts. gRPC silently
// discards an unparseable service config (falling back to NO retries), so a
// JSON typo here would disable transparent UNAVAILABLE replay in production
// without any test or startup failure. Parsing + asserting the shape catches
// that, plus guards the safety + cross-tier-pairing invariants.

type grpcxServiceConfig struct {
	MethodConfig []struct {
		RetryPolicy *struct {
			MaxAttempts          int      `json:"maxAttempts"`
			RetryableStatusCodes []string `json:"retryableStatusCodes"`
		} `json:"retryPolicy"`
	} `json:"methodConfig"`
	RetryThrottling *struct {
		MaxTokens  int     `json:"maxTokens"`
		TokenRatio float64 `json:"tokenRatio"`
	} `json:"retryThrottling"`
}

func TestDefaultServiceConfig_ParsesAndRetriesOnlyUnavailable(t *testing.T) {
	var sc grpcxServiceConfig
	if err := json.Unmarshal([]byte(defaultServiceConfig), &sc); err != nil {
		t.Fatalf("defaultServiceConfig is not valid JSON (gRPC would ship a retry-less channel silently): %v", err)
	}
	if len(sc.MethodConfig) != 1 || sc.MethodConfig[0].RetryPolicy == nil {
		t.Fatalf("want one methodConfig with a retryPolicy; got %+v", sc.MethodConfig)
	}
	// SAFETY: only UNAVAILABLE (request provably never reached the server) is
	// replay-safe for non-idempotent mutations. ABORTED / DEADLINE_EXCEEDED /
	// INTERNAL must stay off the list or a mutation could double-apply.
	codes := sc.MethodConfig[0].RetryPolicy.RetryableStatusCodes
	if len(codes) != 1 || codes[0] != "UNAVAILABLE" {
		t.Errorf("retryableStatusCodes = %v, want exactly [UNAVAILABLE]", codes)
	}
	// Anti-retry-storm token bucket must be present + positive.
	if sc.RetryThrottling == nil || sc.RetryThrottling.MaxTokens <= 0 || sc.RetryThrottling.TokenRatio <= 0 {
		t.Errorf("retryThrottling = %+v, want present with positive maxTokens + tokenRatio", sc.RetryThrottling)
	}
}

// TestDefaultClientKeepalive_PairsWithRuntimeEnforcement pins the keepalive
// against the runtime server's EnforcementPolicy contract documented inline:
// the server permits MinTime=10s, so the client Time must be >= that (and the
// doc caps it at 30s). A client pinging faster than MinTime is killed with
// GOAWAY "too_many_pings" — both sides must move together. PermitWithoutStream
// must be true (idle inter-tier connections carry no active stream).
func TestDefaultClientKeepalive_PairsWithRuntimeEnforcement(t *testing.T) {
	// The runtime EnforcementPolicy MinTime (sdk/go/service/runtime). Kept as a
	// local mirror because the runtime value is a function-local literal, not an
	// exported symbol; this test fails loudly if the client side drifts below it.
	const runtimeServerMinTime = 10 * time.Second

	if defaultClientKeepalive.Time < runtimeServerMinTime {
		t.Errorf("keepalive Time = %v < server MinTime %v — client would be GOAWAY'd with too_many_pings",
			defaultClientKeepalive.Time, runtimeServerMinTime)
	}
	if defaultClientKeepalive.Time != 30*time.Second {
		t.Errorf("keepalive Time = %v, want 30s (the documented paired value)", defaultClientKeepalive.Time)
	}
	if !defaultClientKeepalive.PermitWithoutStream {
		t.Error("PermitWithoutStream = false; idle inter-tier connections would be reaped")
	}
	if defaultClientKeepalive.Timeout <= 0 || defaultClientKeepalive.Timeout >= defaultClientKeepalive.Time {
		t.Errorf("keepalive Timeout = %v, want 0 < Timeout < Time", defaultClientKeepalive.Timeout)
	}
}
