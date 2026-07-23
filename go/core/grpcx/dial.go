package grpcx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// defaultServiceConfig is the channel-level retry service config applied
// to every w17 inter-tier client (business → storage, domain → domain).
// It mirrors lib/grpcclient.DefaultServiceConfig: a dropped backend is
// reconnected transparently by gRPC, but an RPC landing in the window
// before the reconnect completes returns `UNAVAILABLE`; this policy
// retries ONLY that code (the request never reached the server, so
// replay is safe even for mutations) and throttles retries channel-wide
// so a backend outage can't snowball into a retry storm.
const defaultServiceConfig = `{
  "methodConfig": [{
    "name": [{}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }],
  "retryThrottling": {
    "maxTokens": 100,
    "tokenRatio": 0.1
  }
}`

// defaultClientKeepalive keeps an idle inter-tier connection alive
// across NAT / LB idle reapers and detects a half-dead peer mid-call.
// Time is paired with the server-side EnforcementPolicy (MinTime <= 30s,
// PermitWithoutStream true) in sdk/go/service/runtime — pinging faster
// than the server permits triggers a GOAWAY "too_many_pings", so the
// two sides move together.
var defaultClientKeepalive = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// DialOpts returns the standard client dial options for a w17 backend:
// an OTel client stats handler (so client spans chain into the server's
// trace) plus transport credentials — TLS when enabled is true,
// insecure (plain h2c) otherwise.
//
// Generated clients packages pass the result straight into
// Pool.Connect, so the otel + credentials imports live here once
// instead of in every generated package.
//
// When enabled, the TLS posture is refined by the SAME stack-wide
// internal-mesh env knobs that lib/grpcclient / lib/grpcserver read, so
// a business tier dialing its storage tier shares one contract:
//
//	W17_INTERNAL_TLS_CA          — CA bundle to verify the server leaf.
//	                               Unset = system trust roots.
//	W17_INTERNAL_TLS_CERT/_KEY   — this node's leaf, presented only for
//	                               mutual TLS (server pins a client CA).
//
// There is no InsecureSkipVerify: when internal TLS is on, verification
// is always real (dev PKI leaves carry service-name SANs). The enabled
// bool is decided by the caller from the global switch — see
// [InternalTLSEnabled]; the whole stack is TLS or none of it is.
func DialOpts(enabled bool) []grpc.DialOption {
	opts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(forwardPrincipalUnaryInterceptor),
		grpc.WithChainStreamInterceptor(forwardPrincipalStreamInterceptor),
		grpc.WithDefaultServiceConfig(defaultServiceConfig),
		grpc.WithKeepaliveParams(defaultClientKeepalive),
	}
	if !enabled {
		return append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return append(opts, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig(os.Getenv))))
}

// forwardPrincipalUnaryInterceptor relays the gateway-trusted principal
// (`x-w17-user` + `x-w17-scope-*`) from the calling handler's INCOMING
// metadata onto this OUTGOING inter-tier call. It is installed on every
// dial [DialOpts] produces, so a hand-written business-tier handler that
// dials the storage tier passes the caller's identity down automatically
// — the handler writes no plumbing.
//
// Why it is needed: gRPC does not copy a server's incoming metadata to the
// clients it dials, so across a real wire hop the principal would stop at
// the business tier and storage's scope guard would fail closed (a spurious
// PermissionDenied three services from the real cause). A composed binary
// reaches storage in-process (service/inprocgrpc), inheriting the ctx, so
// the principal already rides along there — this closes the standalone/wire
// gap so both deployment topologies behave identically, honouring the
// authz-and-scope contract that identity "flows into every layer".
//
// The relay is scoped to the principal (see [principal.ForwardToOutgoing]):
// paging / tracing / tx-routing metadata are deliberately untouched, and an
// explicit outgoing value the handler set is never overwritten.
func forwardPrincipalUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(principal.ForwardToOutgoing(ctx), method, req, reply, cc, opts...)
}

// forwardPrincipalStreamInterceptor is the streaming twin of
// [forwardPrincipalUnaryInterceptor] — same principal relay, applied when a
// tier opens a streaming RPC on a downstream tier.
func forwardPrincipalStreamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(principal.ForwardToOutgoing(ctx), desc, cc, method, opts...)
}

// InternalTLSEnabled reports whether the stack-wide internal-mesh TLS
// switch (W17_INTERNAL_TLS) is on. Generated business / mcp / grpc-client
// bundles call this to decide the [DialOpts] bool, so every inter-tier
// dialer reads one switch — no per-service knob to diverge. Off is the
// default; the internal mesh is a trusted network the infra secures.
func InternalTLSEnabled() bool {
	return grpcclient.InternalTLSEnabled(os.Getenv(grpcclient.EnvInternalTLS))
}

// clientTLSConfig builds the client tls.Config from the stack-wide
// W17_INTERNAL_TLS_* env knobs. A missing / unreadable CA falls back to
// the system roots rather than erroring — DialOpts has no error channel,
// and a wrong CA path surfaces as a verification failure on the first RPC.
func clientTLSConfig(lookup func(string) string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath := lookup(grpcclient.EnvInternalTLSCA); caPath != "" {
		if pemBytes, err := os.ReadFile(caPath); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pemBytes) {
				cfg.RootCAs = pool
			}
		}
	}
	// Optional mutual TLS: present this node's leaf when both are set.
	cert := lookup(grpcclient.EnvInternalTLSCert)
	key := lookup(grpcclient.EnvInternalTLSKey)
	if cert != "" && key != "" {
		if pair, err := tls.LoadX509KeyPair(cert, key); err == nil {
			cfg.Certificates = []tls.Certificate{pair}
		}
	}
	return cfg
}
