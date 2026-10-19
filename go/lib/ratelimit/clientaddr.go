package ratelimit

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"google.golang.org/grpc/metadata"
)

// HTTPClientAddr is [ClientIP] of an HTTP request — its RemoteAddr and its
// X-Forwarded-For — as text, "" when the peer is no address.
func HTTPClientAddr(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return ClientIP(peer, r.Header.Values("X-Forwarded-For"), trusted).Unmap().String()
}

// GRPCClientAddr is [ClientIP] of a gRPC call — the peer in ctx and the
// incoming x-forwarded-for metadata — as text, "" when ctx has no peer.
func GRPCClientAddr(ctx context.Context, trusted []netip.Prefix) string {
	p := peerIP(ctx)
	if !p.IsValid() {
		return ""
	}
	var forwarded []string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		forwarded = md.Get("x-forwarded-for")
	}
	return ClientIP(p, forwarded, trusted).Unmap().String()
}
