package grpcerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A DB call aborted by ctx cancellation / deadline must
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

// lib/pq answers a context cancelled mid-statement with the server's error
// (57014 query_canceled), which wraps no context error. The request's context
// is what says the caller went away.
func TestWrap_DriverErrorOfACancelledRequest_NoInternalNoReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dbErr := errors.New("pq: canceling statement due to user request")
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(ctx, "AuthQuery.GetTokenWithType", dbErr, nil, DialectPostgres)
	})
	if st, _ := status.FromError(got); st.Code() != codes.Canceled {
		t.Errorf("Code = %v, want Canceled", st.Code())
	}
	if logged != "" {
		t.Errorf("a cancelled request's driver error was reported: %q", logged)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	if st, _ := status.FromError(Wrap(ctx, "M.G", dbErr, nil, DialectPostgres)); st.Code() != codes.DeadlineExceeded {
		t.Errorf("past the request's deadline: Code = %v, want DeadlineExceeded", st.Code())
	}
}

// The same 57014 from Postgres's own statement_timeout, with the request
// still waiting, is a server fault: Internal, and reported.
func TestWrap_StatementTimeoutOfALiveRequest_StillInternal(t *testing.T) {
	var got error
	logged := withCapturedLog(t, func() {
		got = Wrap(context.Background(), "M.G", errors.New("pq: canceling statement due to statement timeout"), nil, DialectPostgres)
	})
	if st, _ := status.FromError(got); st.Code() != codes.Internal {
		t.Errorf("Code = %v, want Internal", st.Code())
	}
	if logged == "" {
		t.Error("a server-side statement timeout was not reported")
	}
}
