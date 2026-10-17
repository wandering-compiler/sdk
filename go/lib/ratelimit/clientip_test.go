package ratelimit

import (
	"net/netip"
	"testing"
)

// ClientIP reads the client as the limiter keys it, but as the address
// itself (no /64 bucket): the peer unless it is a trusted proxy, then the
// right-most untrusted hop.
func TestClientIP(t *testing.T) {
	trusted := ParseTrustedProxies("10.0.0.0/8")
	a := netip.MustParseAddr
	for _, c := range []struct {
		peer string
		xff  []string
		want string
	}{
		{"203.0.113.9", nil, "203.0.113.9"},
		{"203.0.113.9", []string{"1.2.3.4"}, "203.0.113.9"},           // untrusted peer: header ignored
		{"10.0.0.2", []string{"1.2.3.4, 10.0.0.5"}, "1.2.3.4"},        // right-most untrusted hop
		{"10.0.0.2", []string{"2001:db8::1:2"}, "2001:db8::1:2"},      // the address, not its /64
		{"10.0.0.2", []string{"forged, unknown"}, "10.0.0.2"},         // unusable hop: the peer
		{"::ffff:10.0.0.2", []string{"198.51.100.7"}, "198.51.100.7"}, // dual-stack peer
	} {
		if got := ClientIP(a(c.peer), c.xff, trusted); got != a(c.want) {
			t.Errorf("peer %s xff %v: %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}
