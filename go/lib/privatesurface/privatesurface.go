// Package privatesurface is the runtime of a private gRPC surface
// ((w17.private_api)): the gateway another domain's services call, never a
// person.
//
// Every hop is mutual TLS, always — on one machine as across regions:
//
//	caller bundle ──mTLS──▶ private proxy ──mTLS──▶ <domain>-gateway-private
//	(its identity leaf)      (verifies the caller,   (answers the proxy only,
//	                          forwards its identity)  reads the caller from it)
//
// An identity is a certificate's URI SAN, `spiffe://w17/<domain>/<bundle>`.
// The gateway accepts a connection only when the peer certificate is the
// proxy's (EnvProxyID), and reads the caller from the metadata the proxy sets
// from the caller's verified certificate (CallerMetadataKey) — the proxy drops
// any such header a client sends, and nothing but the proxy reaches the
// gateway. Who may call is the surface's `callers` list.
//
// The material is the private PKI's, separate from the mesh switch
// (W17_INTERNAL_TLS): a private surface is mTLS whatever that says.
package privatesurface

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

const (
	// EnvCert / EnvKey are this bundle's identity leaf (PEM): the private
	// gateway serves with it, a caller and the proxy present it.
	EnvCert = "W17_PRIVATE_TLS_CERT"
	EnvKey  = "W17_PRIVATE_TLS_KEY"
	// EnvCA is the private PKI's CA (PEM), which verifies the other side
	// of every hop.
	EnvCA = "W17_PRIVATE_TLS_CA"
	// EnvProxyID is the identity the private gateway accepts connections
	// from: the private proxy's. Unset = DefaultProxyID.
	EnvProxyID = "W17_PRIVATE_PROXY_ID"

	// DefaultProxyID is the private proxy's identity.
	DefaultProxyID = "spiffe://w17/proxy/private"
	// DefaultPort is where a private surface — its proxy and its gateway —
	// listens: not 50051, the usual gRPC port, which a private surface is not.
	DefaultPort = "50061"

	// CallerMetadataKey carries the caller's verified identity from the
	// proxy to the gateway (the proxy sets it from the caller's
	// certificate and drops a client's own).
	CallerMetadataKey = "x-w17-caller"
	// CallerDomainKey is what the gateway tells its backend: the calling
	// domain it admitted. Only the private gateway writes it.
	CallerDomainKey = "x-w17-caller-domain"
)

// Identity is a service identity: the domain and the bundle.
type Identity struct {
	Domain string
	Bundle string
}

// URI is the identity as a certificate carries it.
func (i Identity) URI() string { return "spiffe://w17/" + i.Domain + "/" + i.Bundle }

// ParseIdentity reads `spiffe://w17/<domain>/<bundle>`.
func ParseIdentity(s string) (Identity, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "spiffe" || u.Host != "w17" {
		return Identity{}, fmt.Errorf("identity %q is not spiffe://w17/<domain>/<bundle>", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Identity{}, fmt.Errorf("identity %q is not spiffe://w17/<domain>/<bundle>", s)
	}
	return Identity{Domain: parts[0], Bundle: parts[1]}, nil
}

// LookupFunc reads one environment variable (os.Getenv in production).
type LookupFunc func(string) string

// material loads this bundle's leaf and the CA, all required.
func material(lookup LookupFunc) (tls.Certificate, *x509.CertPool, error) {
	certPath, keyPath, caPath := lookup(EnvCert), lookup(EnvKey), lookup(EnvCA)
	var missing []string
	for k, v := range map[string]string{EnvCert: certPath, EnvKey: keyPath, EnvCA: caPath} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return tls.Certificate{}, nil, fmt.Errorf("private surface: %s not set — a private hop is always mutual TLS (dev PKI: w17/certs/private/)", strings.Join(sortedCopy(missing), ", "))
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("private surface: loading %s/%s: %w", EnvCert, EnvKey, err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("private surface: reading %s at %q: %w", EnvCA, caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("private surface: %s at %q holds no PEM certificate", EnvCA, caPath)
	}
	return cert, pool, nil
}

// ServerTLS is the private gateway's listener: its leaf, and a client
// certificate REQUIRED and verified against the private CA. A missing piece is
// an error — the gateway refuses to boot rather than serve without mTLS.
func ServerTLS(lookup LookupFunc) (*tls.Config, error) {
	cert, pool, err := material(lookup)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

// ClientTLS is a caller's dial to a private surface: its leaf presented, the
// server verified against the private CA as serverName.
func ClientTLS(lookup LookupFunc, serverName string) (*tls.Config, error) {
	cert, pool, err := material(lookup)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
	}, nil
}

// ProxyID is the identity the gateway accepts connections from.
func ProxyID(lookup LookupFunc) string {
	if v := strings.TrimSpace(lookup(EnvProxyID)); v != "" {
		return v
	}
	return DefaultProxyID
}

// ErrNotProxy is a connection whose peer is not the private proxy.
var ErrNotProxy = errors.New("private surface: the connection is not the private proxy's")

// Caller is the caller a request arrived for: the peer must be the private
// proxy (proxyID, verified by the listener's mTLS), and the caller is the
// identity the proxy forwarded. Any failure is Unauthenticated.
func Caller(ctx context.Context, proxyID string) (Identity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, ErrNotProxy.Error())
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates) == 0 {
		return Identity{}, status.Error(codes.Unauthenticated, ErrNotProxy.Error())
	}
	if !hasURI(info.State.PeerCertificates[0], proxyID) {
		return Identity{}, status.Error(codes.Unauthenticated, ErrNotProxy.Error())
	}
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(CallerMetadataKey)
	if len(vals) != 1 {
		return Identity{}, status.Error(codes.Unauthenticated, "private surface: the proxy forwarded no caller identity")
	}
	id, err := ParseIdentity(vals[0])
	if err != nil {
		return Identity{}, status.Error(codes.Unauthenticated, "private surface: "+err.Error())
	}
	return id, nil
}

// ForwardedKeys are the facts a calling gateway states about the person's
// request it serves, which the methods it calls on a private surface read:
// the key a device proof was verified for, the client's address, the host
// and surface it was asked on, the method, the organization selector rule,
// the audience and its membership kinds, the MCP resource, and the product
// asking. The private gateway passes these on from an admitted caller — a
// gateway of an admitted domain, by its certificate — and drops every other
// gateway-owned key it receives (x-w17-user, x-w17-scope-*, labels): those
// are stamped from an auth answer, never stated by a caller.
var ForwardedKeys = []string{
	principal.MintKeyMetadataKey,
	principal.ClientAddrKey,
	"x-w17-method",
	principal.HostKey,
	principal.SurfaceKey,
	"x-w17-mcp-resource",
	principal.TenantFromKey,
	principal.AudienceKey,
	principal.MembershipKindsKey,
	principal.ProductKey,
}

// Forwarded is what the private gateway passes on to its backend of the
// admitted caller's metadata: the ForwardedKeys it sent. A product key naming
// another domain than the caller's is PERMISSION_DENIED — a product asks for
// itself.
func Forwarded(ctx context.Context, caller Identity) (metadata.MD, error) {
	in, _ := metadata.FromIncomingContext(ctx)
	out := metadata.MD{}
	for _, k := range ForwardedKeys {
		vals := in.Get(k)
		if len(vals) == 0 {
			continue
		}
		if k == principal.ProductKey {
			for _, v := range vals {
				if v != caller.Domain {
					return nil, status.Errorf(codes.PermissionDenied, "private surface: domain %q may not ask as product %q", caller.Domain, v)
				}
			}
		}
		out[k] = append([]string(nil), vals...)
	}
	return out, nil
}

// ToBackend is ctx with what the private gateway tells its backend added to
// the outgoing metadata: the calling domain it admitted (CallerDomainKey —
// only this gateway writes it) and the facts the caller stated (Forwarded).
func ToBackend(ctx context.Context, caller Identity, forwarded metadata.MD) context.Context {
	kv := []string{CallerDomainKey, caller.Domain}
	for _, k := range ForwardedKeys {
		for _, v := range forwarded.Get(k) {
			kv = append(kv, k, v)
		}
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

// Admit refuses a caller whose domain is not one of callers.
func Admit(id Identity, callers map[string]bool) error {
	if !callers[id.Domain] {
		return status.Errorf(codes.PermissionDenied, "private surface: domain %q may not call this surface", id.Domain)
	}
	return nil
}

func hasURI(cert *x509.Certificate, want string) bool {
	for _, u := range cert.URIs {
		if u.String() == want {
			return true
		}
	}
	return false
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// Dial connects a caller to a private surface at addr (`<domain>-private:50061`,
// the private proxy): mutual TLS with this bundle's identity, the server
// verified against the private CA under the address's host name. Like every
// inter-tier dial it relays the gateway's stamps from the handler's incoming
// call onto the outgoing one (principal.ForwardToOutgoing): a business tier
// serving a person's request passes on what its gateway stated — the private
// gateway keeps only the ForwardedKeys of it.
func Dial(addr string, lookup LookupFunc) (*grpc.ClientConn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("private surface: address %q: %w", addr, err)
	}
	cfg, err := ClientTLS(lookup, host)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(principal.ForwardToOutgoing(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(principal.ForwardToOutgoing(ctx), desc, cc, method, opts...)
		}),
	)
}

// DefaultAddr is where a caller reaches domain's private surface: the
// private proxy, under the name it answers for that domain.
func DefaultAddr(domain string) string { return domain + "-private:" + DefaultPort }
