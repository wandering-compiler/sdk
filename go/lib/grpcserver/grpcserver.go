// Package grpcserver is the wandering-compiler's unified
// server-side TLS posture helper — the listener-side mirror of
// [grpcclient]. Every generated `main.go` that opens a gRPC
// listener (storage / business / eventbus / composed -server)
// derives its `grpc.ServerOption` credentials from the SAME
// stack-wide switch the dialers read, so:
//
//   - TLS posture is one GLOBAL, all-or-nothing switch
//     ([grpcclient.EnvInternalTLS]) — no per-binary knob a bundle's
//     emitter could set inconsistently. Either the whole internal
//     mesh speaks TLS or none of it does; that matches how the
//     decision is actually made (an infra property of the whole
//     deployment), and it makes "one service diverges" structurally
//     impossible.
//   - The default is OFF (plain h2c): the internal mesh is a trusted
//     network the platform secures (k8s CNI / service mesh), or is
//     deliberately plaintext on a trusted LAN. This mirrors the
//     public edge, whose TLS terminates at the operator's LB/proxy —
//     transport security is infra's job on both surfaces.
//   - Identity is OPT-IN. When the switch is on the default is
//     server-auth TLS: the listener presents its leaf, the dialer
//     verifies it against the (dev or prod) CA. Mutual TLS — verifying
//     the CLIENT's cert too — switches on only when
//     `W17_INTERNAL_TLS_CLIENT_CA` is set, so the common "encrypt the
//     wire" case needs no client certs while a zero-trust boundary can
//     still demand them.
//
// **Bootstrap pattern (generated `main.go`):**
//
//	opt, enabled, err := grpcserver.TLSServerOption(os.Getenv)
//	if err != nil {
//	    return fmt.Errorf("tls: %w", err)
//	}
//	if enabled {
//	    cfg.ServerOptions = append(cfg.ServerOptions, opt)
//	}
//
// A nil `lookup` (or the switch off, the default) returns no TLS
// option — plain h2c. A composed -server reaching its absorbed tiers
// in-process never dials the wire, so it stays plaintext regardless.
package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// LookupFunc is the signature the TLS option uses to read
// environment knobs. Production callers pass `os.Getenv`; tests
// pass a closure over a fixture map.
type LookupFunc func(string) string

// EnvInternalTLSClientCA is the server-only knob that turns on mutual
// TLS: a CA bundle used to VERIFY client certs
// (RequireAndVerifyClientCert). Unset = server-auth TLS only (the
// dialer is not asked for a cert). The switch, server leaf, and CA
// keys are shared with [grpcclient] (EnvInternalTLS, EnvInternalTLSCert,
// EnvInternalTLSKey) so both sides read one contract.
const EnvInternalTLSClientCA = "W17_INTERNAL_TLS_CLIENT_CA"

// TLSServerOption returns the `grpc.ServerOption` carrying the
// listener's TLS posture from the stack-wide switch:
//
//	W17_INTERNAL_TLS            — off (unset / not truthy) → no TLS
//	                              option (plain h2c). "on" → serve TLS.
//	W17_INTERNAL_TLS_CERT       — path to this service's leaf cert (PEM).
//	W17_INTERNAL_TLS_KEY        — path to this service's leaf key  (PEM).
//	                              Both required when the switch is on.
//	W17_INTERNAL_TLS_CLIENT_CA  — set = mutual TLS (verify client certs);
//	                              unset = server-auth TLS only.
//
// Returns (option, true, nil) when TLS is enabled, (nil, false, nil)
// when the switch is off (the default, or a nil lookup), and
// (nil, false, err) on misconfiguration (missing cert/key pair,
// unreadable CA, etc.) so a bad posture fails fast at bootstrap
// instead of at the first handshake.
func TLSServerOption(lookup LookupFunc) (grpc.ServerOption, bool, error) {
	if lookup == nil || !grpcclient.InternalTLSEnabled(lookup(grpcclient.EnvInternalTLS)) {
		return nil, false, nil
	}

	certPath := lookup(grpcclient.EnvInternalTLSCert)
	keyPath := lookup(grpcclient.EnvInternalTLSKey)
	if certPath == "" || keyPath == "" {
		return nil, false, fmt.Errorf(
			"%s=on but %s / %s are not both set — point them at this service's leaf cert+key (dev PKI: w17/certs/dev.local/)",
			grpcclient.EnvInternalTLS, grpcclient.EnvInternalTLSCert, grpcclient.EnvInternalTLSKey)
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, false, fmt.Errorf("loading server cert/key (%s/%s): %w", grpcclient.EnvInternalTLSCert, grpcclient.EnvInternalTLSKey, err)
	}

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}

	if caPath := lookup(EnvInternalTLSClientCA); caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, false, fmt.Errorf("reading %s at %q: %w", EnvInternalTLSClientCA, caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, false, fmt.Errorf("%s at %q: no PEM certs found", EnvInternalTLSClientCA, caPath)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return grpc.Creds(credentials.NewTLS(tlsCfg)), true, nil
}
