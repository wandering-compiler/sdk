package ratelimit

import (
	"bytes"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// A hop that forwards the connection without appending to X-Forwarded-For
// (an L4 balancer, a container runtime's port proxy) presents a private
// address and passes the client's own header through. With the default
// configuration that header must not be believed, or rotating it gives the
// client a fresh bucket per request.
func TestForwardedFromUnconfiguredPrivatePeerIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	l := New(FromEnv("RL_UNSET_PROXY_TEST"))
	l.now = frozenClock()
	allowed := 0
	for i := 0; i < 100; i++ {
		if l.AllowRequest(reqFrom("172.17.0.1:5555", fmt.Sprintf("203.0.113.%d", i))) {
			allowed++
		}
	}
	if allowed != DefaultBurst {
		t.Fatalf("%d/100 requests with a rotated X-Forwarded-For were allowed; want the burst (%d) — the header chose the bucket", allowed, DefaultBurst)
	}
	if got := l.RequestKey(reqFrom("172.17.0.1:5555", "203.0.113.1")); got != "172.17.0.1" {
		t.Errorf("key = %q, want the peer", got)
	}
	if !strings.Contains(buf.String(), "TRUSTED_PROXIES") {
		t.Errorf("an ignored forwarded header from a private peer must name the setting to fix it; log: %q", buf.String())
	}
}

// The old permissive default is still available, explicitly, and can be
// combined with CIDRs.
func TestTrustedProxiesPrivateKeywordIsOptIn(t *testing.T) {
	t.Setenv("KW_RATELIMIT_TRUSTED_PROXIES", "private, 192.0.2.0/24")
	cfg := FromEnv("KW")
	l := New(cfg)
	for _, peer := range []string{"127.0.0.1:1", "10.1.2.3:1", "172.17.0.1:1", "192.168.0.9:1", "192.0.2.7:1", "[::1]:1"} {
		if got := l.RequestKey(reqFrom(peer, "203.0.113.9")); got != "203.0.113.9" {
			t.Errorf("peer %s with private trust: key = %q, want the forwarded client", peer, got)
		}
	}
	if got := l.RequestKey(reqFrom("198.51.100.4:1", "203.0.113.9")); got != "198.51.100.4" {
		t.Errorf("a public peer was trusted: key = %q", got)
	}

	t.Setenv("KW_RATELIMIT_TRUSTED_PROXIES", "none")
	if p := FromEnv("KW").TrustedProxies; len(p) != 0 {
		t.Errorf("none trusts %v", p)
	}
	t.Setenv("KW_RATELIMIT_TRUSTED_PROXIES", "10.0.0.5, not-a-cidr")
	if p := FromEnv("KW").TrustedProxies; len(p) != 1 || p[0] != netip.MustParsePrefix("10.0.0.5/32") {
		t.Errorf("a bare address must trust exactly that host, and a bad entry nothing: %v", p)
	}
}

// Walking from the right, a hop that is not an address ends the walk: what
// lies left of it was not written by a trusted proxy and must not be taken.
func TestForwardedUnparseableHopStopsTheWalk(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, TrustedProxies: prefixes(t, "10.0.0.0/8")})
	if got := l.RequestKey(reqFrom("10.0.0.5:1", "198.51.100.66, unknown")); got != "10.0.0.5" {
		t.Errorf("key = %q; an address left of an unparseable hop was believed", got)
	}
}

// A dual-stack listener reports an IPv4 proxy as ::ffff:a.b.c.d; a
// configured IPv4 prefix must still match it.
func TestTrustedProxyMatchesMappedPeer(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, TrustedProxies: prefixes(t, "10.0.0.0/8")})
	if got := l.RequestKey(reqFrom("[::ffff:10.0.0.5]:1", "203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("key = %q, want the forwarded client", got)
	}
}

// The warning about a private peer's ignored X-Forwarded-For is printed
// once, then at most once an hour, however many requests carry it.
func TestUntrustedForwarderWarningIsHourly(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(FromEnv("RL_HOURLY_WARN_TEST"))
	l.now = func() time.Time { return now }
	warnings := func() int { return strings.Count(buf.String(), "TRUSTED_PROXIES") }
	send := func() {
		for i := 0; i < 20; i++ {
			l.AllowRequest(reqFrom("172.17.0.1:5555", fmt.Sprintf("203.0.113.%d", i)))
		}
	}

	send()
	if n := warnings(); n != 1 {
		t.Fatalf("first burst: %d warnings, want 1", n)
	}
	now = now.Add(59 * time.Minute)
	send()
	if n := warnings(); n != 1 {
		t.Fatalf("within the hour: %d warnings, want still 1", n)
	}
	now = now.Add(2 * time.Minute)
	send()
	if n := warnings(); n != 2 {
		t.Fatalf("after the hour: %d warnings, want 2", n)
	}
}
