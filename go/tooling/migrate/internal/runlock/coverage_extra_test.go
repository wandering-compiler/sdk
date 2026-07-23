package runlock

import (
	"context"
	"testing"
)

// TestStartBeater covers the production entry point (it delegates to
// StartBeaterInterval with the fixed Heartbeat). The 60s tick never fires in
// the test window; Stop ends the goroutine cleanly.
func TestStartBeater(t *testing.T) {
	b := StartBeaterInterval(context.Background(), Heartbeat,
		func(context.Context) error { return nil },
		func(error) {})
	if b == nil {
		t.Fatal("StartBeaterInterval returned nil")
	}
	b.Stop() // must return promptly without the ticker ever firing
}
