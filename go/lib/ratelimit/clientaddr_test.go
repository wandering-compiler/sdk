package ratelimit

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Behind a trusted proxy the caller is the right-most untrusted hop; without
// one, the peer — whatever X-Forwarded-For the caller wrote.
func TestClientAddr_HTTPAndGRPC(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7")
	if got := HTTPClientAddr(r, trusted); got != "203.0.113.7" {
		t.Errorf("behind the proxy = %q", got)
	}
	r.RemoteAddr = "198.51.100.9:1234"
	if got := HTTPClientAddr(r, trusted); got != "198.51.100.9" {
		t.Errorf("not behind it = %q", got)
	}
	// A zone names an interface of this machine; it is dropped.
	r.RemoteAddr = "[fe80::1%eth0]:1234"
	if got := HTTPClientAddr(r, trusted); got != "fe80::1" {
		t.Errorf("zoned peer = %q", got)
	}

	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1}})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", "203.0.113.7"))
	if got := GRPCClientAddr(ctx, trusted); got != "203.0.113.7" {
		t.Errorf("grpc behind the proxy = %q", got)
	}
	if got := GRPCClientAddr(context.Background(), trusted); got != "" {
		t.Errorf("grpc without a peer = %q", got)
	}
}
