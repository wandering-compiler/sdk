package restgw_test

import (
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestStreamWriteTimeoutFromEnv — unset / unparseable fall back to the 30s
// default; an explicit positive value is N seconds; <= 0 opts out (0 = no
// deadline).
func TestStreamWriteTimeoutFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	const key = "APP_STREAM_WRITE_TIMEOUT_SECONDS"

	if got := restgw.StreamWriteTimeoutFromEnv("APP", env(nil)); got != 30*time.Second {
		t.Errorf("unset = %v, want 30s default", got)
	}
	if got := restgw.StreamWriteTimeoutFromEnv("APP", env(map[string]string{key: "notanumber"})); got != 30*time.Second {
		t.Errorf("unparseable = %v, want 30s default", got)
	}
	if got := restgw.StreamWriteTimeoutFromEnv("APP", env(map[string]string{key: "5"})); got != 5*time.Second {
		t.Errorf("explicit 5 = %v, want 5s", got)
	}
	if got := restgw.StreamWriteTimeoutFromEnv("APP", env(map[string]string{key: "0"})); got != 0 {
		t.Errorf("zero = %v, want 0 (opt-out)", got)
	}
	if got := restgw.StreamWriteTimeoutFromEnv("APP", env(map[string]string{key: "-3"})); got != 0 {
		t.Errorf("negative = %v, want 0 (opt-out)", got)
	}
}
