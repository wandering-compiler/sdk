package grpcerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// R-grpcerr-1: a DB call aborted by ctx cancellation / deadline must
// map to the canonical gRPC code (Canceled / DeadlineExceeded) and
// must NOT be reported to observx as an internal error (that path
// generated Sentry noise on every client disconnect / slow query).

func TestWrap_ContextCanceled_NoInternalNoReport(t *testing.T) {
	// Wrap the sentinel so we also exercise errors.Is matching, the
	// way a real driver surfaces it.
	dbErr := fmt.Errorf("query failed: %w", context.Canceled)
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "UserQuery.List", dbErr, nil, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.Canceled {
		t.Errorf("Code = %v, want Canceled", st.Code())
	}
	// Must not route through observx (no log line in test fallback).
	if logged != "" {
		t.Errorf("context.Canceled must not be reported as internal error; logged: %q", logged)
	}
}

func TestWrap_ContextDeadlineExceeded_NoInternalNoReport(t *testing.T) {
	dbErr := fmt.Errorf("statement timeout: %w", context.DeadlineExceeded)
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "UserQuery.List", dbErr, nil, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("Code = %v, want DeadlineExceeded", st.Code())
	}
	if logged != "" {
		t.Errorf("context.DeadlineExceeded must not be reported as internal error; logged: %q", logged)
	}
}

func TestWrap_ContextCanceled_DirectSentinel(t *testing.T) {
	got := Wrap(context.Background(), "M.G", context.Canceled, nil, DialectPostgres)
	st, _ := status.FromError(got)
	if st.Code() != codes.Canceled {
		t.Errorf("Code = %v, want Canceled", st.Code())
	}
}

// Guard: a genuine non-context error still routes to Internal +
// observx — the cancellation short-circuit must not swallow real
// failures.
func TestWrap_NonContextError_StillInternal(t *testing.T) {
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", errors.New("connection reset"), nil, DialectPostgres)
	})
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal {
		t.Errorf("Code = %v, want Internal", st.Code())
	}
	if logged == "" {
		t.Error("genuine internal error should still be reported")
	}
}
