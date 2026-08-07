package grpcerr

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// withCapturedLog redirects the standard `log` package's
// output (which is what gox/errorx falls back to without an
// explicit reporter) to a buffer + returns the captured
// output. Used to assert RecoverPanic / Wrap routing without
// polluting test output.
//
// Note: we don't capture os.Stderr directly — log's default
// writer is cached at package init, so swapping os.Stderr
// after that point doesn't redirect log output.
func withCapturedLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)
	fn()
	return buf.String()
}

func TestRecoverPanic_AssignsInternalStatus(t *testing.T) {
	var capturedErr error
	logged := withCapturedLog(t, func() {
		func() {
			defer RecoverPanic(context.Background(), "UserMutation.CreateUser", &capturedErr)
			panic("boom: something went wrong internally")
		}()
	})

	st, ok := status.FromError(capturedErr)
	if !ok {
		t.Fatalf("expected gRPC status, got %T %v", capturedErr, capturedErr)
	}
	if st.Code() != codes.Internal {
		t.Errorf("Code = %v, want Internal", st.Code())
	}
	if !strings.Contains(st.Message(), "UserMutation.CreateUser") {
		t.Errorf("status message must carry method identity: %q", st.Message())
	}
	// Panic value MUST NOT leak to the client — message stays
	// generic.
	if strings.Contains(st.Message(), "boom") {
		t.Errorf("panic value leaked to client: %q", st.Message())
	}
	// Internal log SHOULD carry the panic value + method —
	// errorx routes to log.Printf in absence of explicit
	// reporter; format is `error: PANIC <method>: <value>...`.
	if !strings.Contains(logged, "PANIC UserMutation.CreateUser") {
		t.Errorf("log should carry PANIC line: %q", logged)
	}
	if !strings.Contains(logged, "boom") {
		t.Errorf("log should carry panic value: %q", logged)
	}
}

func TestRecoverPanic_NoPanicNoOp(t *testing.T) {
	var capturedErr error
	logged := withCapturedLog(t, func() {
		func() {
			defer RecoverPanic(context.Background(), "M", &capturedErr)
			// no panic
		}()
	})
	if capturedErr != nil {
		t.Errorf("RecoverPanic should not assign on no-panic; got %v", capturedErr)
	}
	if logged != "" {
		t.Errorf("RecoverPanic should not log on no-panic; got %q", logged)
	}
}

func TestRecoverPanic_NilErrOutSurvives(t *testing.T) {
	// Passing nil errOut is defensive — RecoverPanic must not
	// panic on assignment when caller passes nil.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RecoverPanic itself should not panic on nil errOut: %v", r)
		}
	}()
	withCapturedLog(t, func() {
		func() {
			defer RecoverPanic(context.Background(), "M", nil)
			panic("test")
		}()
	})
}

func TestRecoverPanic_ErrorPanicValue(t *testing.T) {
	// Panicking with an error value (vs string) — common Go
	// pattern. Recovery shape stays identical.
	var capturedErr error
	withCapturedLog(t, func() {
		func() {
			defer RecoverPanic(context.Background(), "M", &capturedErr)
			panic(errors.New("structured error panic"))
		}()
	})
	st, _ := status.FromError(capturedErr)
	if st.Code() != codes.Internal {
		t.Errorf("Code = %v", st.Code())
	}
}
