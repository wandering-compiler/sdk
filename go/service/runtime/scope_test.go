package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// Unguarded reflection is refused at start through the front door too.
func TestGRPCComponent_UnguardedReflectionIsRefused(t *testing.T) {
	err := GRPCComponent(Config{
		Addr:             "127.0.0.1:0",
		Reflection:       true,
		RegisterServices: func(*grpc.Server) error { return nil },
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ReflectionAuth") {
		t.Fatalf("Run = %v, want the reflection refusal", err)
	}
}

// The recovery interceptors give each request its own error-tracker scope,
// so breadcrumbs recorded while serving it stay with it.
func TestRecoveryInterceptors_GiveEachRequestItsOwnScope(t *testing.T) {
	if err := observx.MustInit(observx.Config{ServiceName: "svc", SentryDSN: "https://abc123@127.0.0.1/1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observx.Shutdown(ctx)
	})

	var hubs []*sentry.Hub
	unary := func(ctx context.Context, _ any) (any, error) {
		hubs = append(hubs, sentry.GetHubFromContext(ctx))
		return nil, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := recoveryUnaryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/s/M"}, unary); err != nil {
			t.Fatal(err)
		}
	}
	stream := func(_ any, ss grpc.ServerStream) error {
		hubs = append(hubs, sentry.GetHubFromContext(ss.Context()))
		return nil
	}
	if err := recoveryStreamInterceptor(nil, ctxStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/s/S"}, stream); err != nil {
		t.Fatal(err)
	}
	if len(hubs) != 3 || hubs[0] == nil || hubs[1] == nil || hubs[2] == nil {
		t.Fatalf("a request ran without its own hub: %v", hubs)
	}
	if hubs[0] == hubs[1] || hubs[0] == sentry.CurrentHub() || hubs[2] == sentry.CurrentHub() {
		t.Error("requests share a hub")
	}
}

// ctxStream is a grpc.ServerStream carrying only a context.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context     { return s.ctx }
func (s ctxStream) SetHeader(metadata.MD) error  { return nil }
func (s ctxStream) SendHeader(metadata.MD) error { return nil }
func (s ctxStream) SetTrailer(metadata.MD)       {}
