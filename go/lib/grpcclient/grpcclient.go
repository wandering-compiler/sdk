// Package grpcclient is the wandering-compiler's unified
// client-side gRPC dial helper. Both generated `main.go` files
// and authored plugins call into this single Dial path so:
//
//   - TLS posture is a single STACK-WIDE switch, not a per-binary
//     knob. Encrypting the internal service mesh is an infra-shaped,
//     all-or-nothing property of the whole deployment (see
//     [TLSDialOption]); the dial helper reads one global env var,
//     so no service can silently diverge from its neighbours.
//   - W3C trace context propagates across every dial out of the
//     box via the standard `otelgrpc.NewClientHandler()` stats
//     handler — spans chain into whatever OTel pipeline
//     `lib/observx` configured at boot.
//   - resiliency is ON by default and uniform across every w17
//     client: a retry service config ([DefaultServiceConfig])
//     transparently replays `UNAVAILABLE` RPCs — the one code that
//     means the request never reached the server, so replay is safe
//     even for non-idempotent mutations — with a channel-level
//     throttle that stops a backend outage being amplified into a
//     retry storm; and application keepalive
//     ([DefaultClientKeepalive]) holds an idle service-to-service
//     connection open across NAT / LB / firewall idle reapers and
//     detects a half-dead peer mid-call. Both are PAIRED with the
//     server side (`sdk/go/service/runtime` + the generated
//     gateway/console servers set a matching `EnforcementPolicy`) —
//     a client pinging faster than the server permits is killed
//     with GOAWAY "too_many_pings", so the two sides move together.
//     Callers append `opts` to override either (last DialOption wins).
//
// **Sharing.** Today every binary holds one `*grpc.ClientConn`
// per backend. HTTP/2 multiplexing means one connection handles
// thousands of concurrent streams without issue, so the
// "shared per (host:port)" pattern is a natural lift point —
// but premature today (each w17 binary has exactly one
// backend). Keeping the surface as `Dial(addr, ...)` lets a
// future `Pool` slot in behind the same name without breaking
// callers.
//
// **Bootstrap pattern (generated `main.go` + plugin alike):**
//
//	conn, err := grpcclient.Dial(backendAddr, os.Getenv)
//	if err != nil {
//	    log.Fatalf("dial %s: %v", backendAddr, err)
//	}
//	defer conn.Close()
//
// The TLS posture comes from the stack-wide `W17_INTERNAL_TLS`
// switch read through `lookup` — see [TLSDialOption]. A nil
// `lookup` (or the switch off, the default) dials plain h2c: the
// internal mesh is a trusted network the infra secures, so
// plaintext between services is a legitimate default, not an
// accident.
package grpcclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// DefaultServiceConfig is the channel-level gRPC service config
// applied to every w17 client connection. gRPC already reconnects a
// dropped backend transparently (connection-level exponential
// backoff), but an RPC that lands on the connection in the window
// BEFORE the reconnect completes returns `UNAVAILABLE` — and without a
// retry policy that error escapes to the caller even though the very
// next dial would succeed. The policy retries ONLY `UNAVAILABLE`: it
// means the request never reached the server, so replay is safe even
// for non-idempotent mutations. Codes where the server may have
// already applied the write (`ABORTED`, `DEADLINE_EXCEEDED`, …) are
// deliberately NOT retried. `retryThrottling` is a channel-level token
// bucket — sustained failures drain it and suspend retries, so a
// backend outage can't be amplified into a retry storm.
const DefaultServiceConfig = `{
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

// DefaultClientKeepalive keeps an idle service-to-service connection
// alive across NAT / LB / firewall idle reapers and detects a
// half-dead peer during a long-running call (the connection is torn
// after Timeout with no ping ack). `Time` is paired with the
// server-side `keepalive.EnforcementPolicy` (`MinTime` <= 30s,
// `PermitWithoutStream` true) — a client pinging faster than the
// server permits is killed with GOAWAY "too_many_pings", so both sides
// MUST move together.
var DefaultClientKeepalive = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// LookupFunc is the signature the TLS option uses to read
// environment knobs. Production callers pass `os.Getenv`;
// tests pass a closure over a fixture map.
type LookupFunc func(string) string

// Internal-mesh TLS env keys — a single, GLOBAL (non-prefixed) set
// read identically by every w17 binary, client and [grpcserver]
// side alike, so the whole stack shares one posture. Encrypting the
// internal mesh is an all-or-nothing infra decision; there is
// deliberately no per-service knob to forget.
const (
	// EnvInternalTLS is the stack-wide switch. "on"/"true"/"1"/"yes"
	// turns internal TLS on; unset or anything else = off (default).
	EnvInternalTLS = "W17_INTERNAL_TLS"
	// EnvInternalTLSCA is the CA bundle a CLIENT uses to verify the
	// server leaf (RootCAs). Unset = system trust roots (prod PKI).
	EnvInternalTLSCA = "W17_INTERNAL_TLS_CA"
	// EnvInternalTLSCert / EnvInternalTLSKey are THIS node's leaf. A
	// server presents it; a client presents it only under mTLS (when
	// the server pins a client CA — see [grpcserver]).
	EnvInternalTLSCert = "W17_INTERNAL_TLS_CERT"
	EnvInternalTLSKey  = "W17_INTERNAL_TLS_KEY"
)

// InternalTLSEnabled reports whether the stack-wide internal-TLS
// switch ([EnvInternalTLS]) is on. Shared spelling so the client and
// [grpcserver] agree on what "on" means.
func InternalTLSEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// Dial opens a non-blocking client connection to addr. Wires
// the convention-driven dial options first (TLS posture from the
// stack-wide `W17_INTERNAL_TLS` switch + otelgrpc trace propagation),
// then appends `opts` so callers can layer per-call overrides
// (UserAgent, custom interceptors, …).
//
// Returns the connection in the IDLE state — gRPC's standard
// non-blocking behaviour. The first RPC triggers the dial; if
// the backend is down at startup, that first RPC carries the
// `Unavailable` error rather than `main.go` exploding.
//
// `lookup` is the env reader. Pass `os.Getenv` in production;
// any closure works in tests. Nil `lookup` defaults to
// `os.Getenv` so callers without a test seam stay terse.
//
// Errors only on TLS env misconfig (partial mTLS pair, missing
// CA file, etc.). Returns the error verbatim so the caller
// surfaces it with their own context.
func Dial(addr string, lookup LookupFunc, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	tlsOpt, err := TLSDialOption(lookup)
	if err != nil {
		return nil, err
	}
	dialOpts := append([]grpc.DialOption{
		tlsOpt,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultServiceConfig(DefaultServiceConfig),
		grpc.WithKeepaliveParams(DefaultClientKeepalive),
	}, opts...)
	return grpc.NewClient(addr, dialOpts...)
}

// TLSDialOption returns one DialOption carrying the internal-mesh
// TLS posture, driven by the STACK-WIDE switch (not a per-service
// knob):
//
//	W17_INTERNAL_TLS        — off (unset / not truthy) → plain h2c.
//	                          "on" → dial TLS, verifying the server.
//	W17_INTERNAL_TLS_CA     — CA bundle to verify the server leaf
//	                          (RootCAs). Unset = system trust roots.
//	W17_INTERNAL_TLS_CERT   — this node's leaf cert, presented only
//	W17_INTERNAL_TLS_KEY      when the server pins a client CA (mTLS).
//
// Why a global switch: whether the internal mesh is encrypted is an
// infra-shaped, all-or-nothing property of the deployment — the whole
// network is either secured by the platform (k8s CNI / service mesh,
// so app-level TLS is redundant), deliberately plaintext on a trusted
// LAN, or hardened end to end. Half the services doing TLS and half
// not is never a real configuration; it is only a bug (a bundle whose
// emitter forgot a knob). So there is ONE switch, read identically by
// every binary, and NO InsecureSkipVerify: if you turn internal TLS
// on you have real, SAN-correct leaves (dev PKI or prod CA) and verify
// them properly.
//
// Off is the DEFAULT and a legitimate posture, not an accident — the
// public edge (REST/gRPC gateway) terminates its TLS at the operator's
// LB/proxy exactly the same way; internal hops follow the same "infra
// owns transport security" model. A composed single-binary reaches its
// absorbed tiers in-process, never touches the wire, and so stays
// plaintext regardless of the switch.
//
// Errors when env vars are partially set (mTLS cert without key,
// unreadable CA, etc.) so misconfiguration surfaces at startup instead
// of at dial-time. Exposed separately from [Dial] for callers that own
// their full DialOption list and just need the TLS slot.
func TLSDialOption(lookup LookupFunc) (grpc.DialOption, error) {
	if lookup == nil || !InternalTLSEnabled(lookup(EnvInternalTLS)) {
		// Switch off (default): plain h2c. The internal mesh is trusted
		// / infra-secured; plaintext between services is intended here.
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if caPath := lookup(EnvInternalTLSCA); caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s at %q: %w", EnvInternalTLSCA, caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("%s at %q: no PEM certs found", EnvInternalTLSCA, caPath)
		}
		tlsCfg.RootCAs = pool
	}

	// Optional mTLS: present this node's leaf so a zero-trust server
	// (one that pins a client CA) can verify the dialer too. Server-auth
	// TLS — the common "encrypt the wire" case — needs no client cert.
	clientCert := lookup(EnvInternalTLSCert)
	clientKey := lookup(EnvInternalTLSKey)
	switch {
	case clientCert != "" && clientKey != "":
		cert, err := tls.LoadX509KeyPair(clientCert, clientKey)
		if err != nil {
			return nil, fmt.Errorf("loading mTLS client cert/key (%s/%s): %w", EnvInternalTLSCert, EnvInternalTLSKey, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	case clientCert != "" || clientKey != "":
		return nil, fmt.Errorf("%s and %s must be set together (mTLS needs both)", EnvInternalTLSCert, EnvInternalTLSKey)
	}

	return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}
