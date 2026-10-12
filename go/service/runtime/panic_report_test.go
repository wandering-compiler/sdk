package runtime

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A handler panic reaches the operator with its value and stack, and the
// client only with a generic Internal naming the method.
func TestRecoveryUnaryInterceptor_ReportsTheStackAndTellsTheClientNothing(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	_, err := recoveryUnaryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/shop.Orders/Pay"},
		func(context.Context, any) (any, error) { panic("card vault key: sk_live_123") })

	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "sk_live") || !strings.Contains(msg, "internal error") {
		t.Errorf("client message = %q, want a generic sentence without the panic value", msg)
	}
	report := buf.String()
	if !strings.Contains(report, "sk_live_123") || !strings.Contains(report, "/shop.Orders/Pay") {
		t.Errorf("the report lacks the panic value or the method: %q", report)
	}
	if !strings.Contains(report, "goroutine") || !strings.Contains(report, "panic_report_test.go") {
		t.Errorf("the report carries no stack: %q", report)
	}
}
