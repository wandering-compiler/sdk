package dpopclient_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

const grpcOrigin = "https://api.example.com"

func payloadOf(t *testing.T, proof string) []byte {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWS: %q", proof)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// grpcServer verifies every call with nonces on; the challenge travels in the
// trailer via dpop.ChallengeMetadata.
type grpcServer struct {
	v           *dpop.Verifier
	binding     string
	hits        atomic.Int32
	alwaysNonce atomic.Bool
	refuse      atomic.Value // string: a w17.ErrorDetail code to refuse with
	lastMD      atomic.Value // metadata.MD
}

func (g *grpcServer) check(ctx context.Context, fullMethod string) error {
	g.hits.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	g.lastMD.Store(md)
	tok := ""
	if v := md.Get(dpop.MetadataAuthorization); len(v) == 1 {
		tok, _ = dpop.AccessToken(v[0])
	}
	binding := ""
	if tok != "" {
		binding = g.binding
	}
	tp, err := g.v.Verify(ctx, dpop.Request{
		Method: "POST", Path: fullMethod, Proofs: md.Get(dpop.MetadataProof), AccessToken: tok, Binding: binding,
	})
	if err == nil && g.alwaysNonce.Load() {
		n, _ := g.v.Nonce(tp + string('a'+g.hits.Load()))
		err = &dpop.NonceError{Nonce: n}
	}
	if err != nil {
		_ = grpc.SetTrailer(ctx, dpop.ChallengeMetadata(err))
		return status.Error(codes.Unauthenticated, err.Error())
	}
	if code, _ := g.refuse.Load().(string); code != "" {
		st, _ := status.New(codes.Unauthenticated, "refused").WithDetails(&w17pb.ErrorDetail{Code: code, Message: "refused"})
		return st.Err()
	}
	return nil
}

func startGRPC(t *testing.T, binding string) (*grpcServer, *bufconn.Listener) {
	t.Helper()
	nonces, err := dpop.NewHMACNonces(nonceKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := dpop.NewVerifier(dpop.Config{Origin: grpcOrigin, Nonces: nonces})
	if err != nil {
		t.Fatal(err)
	}
	g := &grpcServer{v: v, binding: binding}
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := g.check(ctx, info.FullMethod); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := g.check(ss.Context(), info.FullMethod); err != nil {
				return err
			}
			return h(srv, ss)
		}),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return g, lis
}

func dial(t *testing.T, lis *bufconn.Listener, opts ...grpc.DialOption) healthpb.HealthClient {
	t.Helper()
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	cc, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return healthpb.NewHealthClient(cc)
}

func newCreds(k *dpop.Key) *dpopclient.Credentials {
	return &dpopclient.Credentials{Signer: k, Origin: grpcOrigin, Token: token("tok"), AllowInsecure: true}
}

// A unary call without a nonce is refused with use_dpop_nonce in the trailer;
// the interceptor retries once with the nonce, and the next call carries it
// from the start. The htu is the configured origin + full method name.
func TestCredentials_UnaryNonceRetryOnce(t *testing.T) {
	k := mustKey(t)
	g, lis := startGRPC(t, k.Thumbprint())
	hc := dial(t, lis, newCreds(k).DialOptions()...)
	ctx := context.Background()
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if g.hits.Load() != 2 {
		t.Fatalf("first call took %d attempts, want 2", g.hits.Load())
	}
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if g.hits.Load() != 3 {
		t.Fatalf("second call took %d attempts, want 1", g.hits.Load()-2)
	}
	md := g.lastMD.Load().(metadata.MD)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "DPoP tok" {
		t.Fatalf("authorization = %v", got)
	}
	p := string(payloadOf(t, md.Get("dpop")[0]))
	if !strings.Contains(p, `"htu":"https://api.example.com/grpc.health.v1.Health/Check"`) || !strings.Contains(p, `"htm":"POST"`) {
		t.Fatalf("proof claims = %s", p)
	}
}

func TestCredentials_UnaryRetriesExactlyOnce(t *testing.T) {
	k := mustKey(t)
	g, lis := startGRPC(t, k.Thumbprint())
	g.alwaysNonce.Store(true)
	hc := dial(t, lis, newCreds(k).DialOptions()...)
	_, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if status.Code(err) != codes.Unauthenticated || g.hits.Load() != 2 {
		t.Fatalf("err %v after %d attempts, want Unauthenticated after 2", err, g.hits.Load())
	}
}

// A stream is not retried, but the nonce from its refusal is learned and the
// next stream succeeds first time.
func TestCredentials_StreamLearnsNonce(t *testing.T) {
	k := mustKey(t)
	g, lis := startGRPC(t, k.Thumbprint())
	hc := dial(t, lis, newCreds(k).DialOptions()...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := hc.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first stream = %v, want Unauthenticated", err)
	}
	st, err = hc.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if g.hits.Load() != 2 {
		t.Fatalf("%d attempts, want 2", g.hits.Load())
	}
}

// DEVICE_REVOKED / SESSION_EXPIRED refusals come back typed, with the status
// still readable.
func TestCredentials_RefusalTyped(t *testing.T) {
	k := mustKey(t)
	g, lis := startGRPC(t, k.Thumbprint())
	hc := dial(t, lis, newCreds(k).DialOptions()...)
	ctx := context.Background()
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	g.refuse.Store("DEVICE_REVOKED")
	_, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
	var re *dpopclient.RefusedError
	if !errors.As(err, &re) || re.Reason != dpopclient.ReasonDeviceRevoked || !errors.Is(err, dpopclient.ErrDeviceRevoked) {
		t.Fatalf("refusal = %v", err)
	}
	if status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != "refused" {
		t.Fatalf("status lost: %v", status.Convert(err))
	}
	g.refuse.Store("SESSION_EXPIRED")
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{}); !errors.Is(err, dpopclient.ErrSessionExpired) {
		t.Fatalf("session refusal = %v", err)
	}
	g.refuse.Store("SOMETHING_ELSE")
	_, err = hc.Check(ctx, &healthpb.HealthCheckRequest{})
	if errors.As(err, &re) || status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown reason = %v (%T), want the plain status", err, err)
	}
}

// Without AllowInsecure the credentials refuse to travel in clear text.
func TestCredentials_RequireTransportSecurity(t *testing.T) {
	c := newCreds(mustKey(t))
	c.AllowInsecure = false
	if !c.RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity = false by default")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, c.DialOptions()...)
	if cc, err := grpc.NewClient("passthrough:///bufnet", opts...); err == nil {
		_ = cc.Close()
		t.Fatal("an insecure connection accepted secure-only credentials")
	}
}
