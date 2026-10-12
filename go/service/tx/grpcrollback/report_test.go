package grpcrollback_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A rollback that fails is reported; one that finds the transaction
// already drained is not; the handler's error comes back unchanged either
// way.
func TestInterceptor_ReportsAFailedRollbackOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rollback error
		reported bool
	}{
		{"failed", errors.New("connection reset"), true},
		{"already drained", txregistry.ErrUnknownTxID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(orig)

			handlerErr := errors.New("handler failed")
			_, err := grpcrollback.Interceptor(&fakeRoller{err: tc.rollback})(
				ctxWithTxID("tx-1"), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/Write"},
				func(_ context.Context, _ any) (any, error) { return nil, handlerErr })
			if err != handlerErr {
				t.Errorf("returned %v, want the handler's error unchanged", err)
			}
			got := strings.Contains(buf.String(), "automatic rollback failed")
			if got != tc.reported {
				t.Errorf("reported = %v, want %v; log: %q", got, tc.reported, buf.String())
			}
		})
	}
}
