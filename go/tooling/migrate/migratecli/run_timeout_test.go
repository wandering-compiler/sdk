package migratecli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// apply and rollback run under a deadline the operator can set: a fixed 30
// minutes stopped a long but healthy migration mid-body (on MySQL mid-DDL,
// the loud wedge) with no way to ask for more. --timeout 0 is no deadline.
func TestApplyAndRollback_RunDeadlineIsTheOperators(t *testing.T) {
	f, err := parseFlags("apply", []string{"--timeout", "2h"}, &strings.Builder{})
	if err != nil {
		t.Fatalf("apply --timeout 2h: %v", err)
	}
	ctx, cancel := runContext(context.Background(), f.timeout)
	dl, ok := ctx.Deadline()
	cancel()
	if !ok || time.Until(dl) < 119*time.Minute {
		t.Errorf("--timeout 2h gave a deadline %v away (set=%v)", time.Until(dl), ok)
	}

	f, err = parseFlags("rollback", []string{"--to", "", "--timeout", "0"}, &strings.Builder{})
	if err != nil {
		t.Fatalf("rollback --timeout 0: %v", err)
	}
	ctx, cancel = runContext(context.Background(), f.timeout)
	_, ok = ctx.Deadline()
	cancel()
	if ok {
		t.Error("--timeout 0 still set a deadline")
	}

	f, err = parseFlags("apply", nil, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if f.timeout != 30*time.Minute {
		t.Errorf("the default run deadline is %v, want 30m", f.timeout)
	}
	if _, err := parseFlags("status", []string{"--timeout", "1m"}, &strings.Builder{}); err == nil {
		t.Error("status accepted --timeout, which it does not read")
	}
}
