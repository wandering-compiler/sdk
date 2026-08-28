package ratelimit

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("bad prefix %q: %v", s, err)
		}
		out = append(out, p)
	}
	return out
}

// ctxFrom builds an incoming context as gRPC would present it: a peer
// address, plus whatever X-Forwarded-For the hop in front appended.
func ctxFrom(peerAddr string, xff ...string) context.Context {
	ctx := context.Background()
	if peerAddr != "" {
		host, portStr, err := net.SplitHostPort(peerAddr)
		if err != nil {
			host, portStr = peerAddr, "1234"
		}
		port := 1234
		if p, err := net.LookupPort("tcp", portStr); err == nil {
			port = p
		}
		ctx = peer.NewContext(ctx, &peer.Peer{
			Addr: &net.TCPAddr{IP: net.ParseIP(host), Port: port},
		})
	}
	if len(xff) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", xff[0]))
	}
	return ctx
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if !l.Allow(context.Background()) {
		t.Fatal("nil limiter must allow")
	}
}

func TestDisabledByNonPositiveRate(t *testing.T) {
	if New(Config{PerMinute: 0}) != nil {
		t.Fatal("PerMinute 0 must disable")
	}
}

func TestBurstThenDeny(t *testing.T) {
	// 60/min = 1/s, burst 3: three immediate calls pass, the fourth does not.
	l := New(Config{PerMinute: 60, Burst: 3})
	frozen := time.Now()
	l.now = func() time.Time { return frozen }

	for i := 0; i < 3; i++ {
		if !l.allowKey("a") {
			t.Fatalf("call %d within burst was denied", i+1)
		}
	}
	if l.allowKey("a") {
		t.Fatal("call past the burst was allowed")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	// The whole point of per-key buckets: one caller exhausting its budget
	// must not deny anyone else. A global cap would fail exactly here.
	l := New(Config{PerMinute: 60, Burst: 2})
	for i := 0; i < 2; i++ {
		l.allowKey("attacker")
	}
	if l.allowKey("attacker") {
		t.Fatal("attacker should be exhausted")
	}
	if !l.allowKey("victim") {
		t.Fatal("a second key was denied by the first key's traffic")
	}
}

// The spoofing case. An UNTRUSTED peer sending X-Forwarded-For must not be
// able to choose its bucket — otherwise a header rotation defeats the limit
// entirely and the whole package is decoration.
func TestForwardedIgnoredFromUntrustedPeer(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, TrustedProxies: prefixes(t, "10.0.0.0/8")})

	k1 := l.ClientKey(ctxFrom("203.0.113.7:5000", "198.51.100.1"))
	k2 := l.ClientKey(ctxFrom("203.0.113.7:5000", "198.51.100.2"))

	if k1 != "203.0.113.7" {
		t.Fatalf("untrusted peer's header was believed: key=%q", k1)
	}
	if k1 != k2 {
		t.Fatalf("rotating the header changed the bucket: %q vs %q", k1, k2)
	}
}

func TestForwardedHonouredFromTrustedProxy(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, TrustedProxies: prefixes(t, "10.0.0.0/8")})
	if got := l.ClientKey(ctxFrom("10.110.0.5:5000", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("trusted proxy's header ignored: key=%q", got)
	}
}

// Rightmost-untrusted, not leftmost. With two trusted hops appended, the
// real client is the last entry that is not itself a proxy. Reading left to
// right would return whatever the CLIENT put there first — here a forged
// address that must not win.
func TestForwardedPicksRightmostUntrusted(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, TrustedProxies: prefixes(t, "10.0.0.0/8")})
	ctx := ctxFrom("10.110.0.5:5000", "198.51.100.66, 203.0.113.9, 10.110.0.4")
	if got := l.ClientKey(ctx); got != "203.0.113.9" {
		t.Fatalf("wrong hop chosen: key=%q (leftmost would give 198.51.100.66)", got)
	}
}

func TestUnknownPeerSharesOneBucket(t *testing.T) {
	// An unreadable address must not mint a fresh unmetered bucket per call.
	l := New(Config{PerMinute: 60, Burst: 5})
	if got := l.ClientKey(context.Background()); got != "unknown" {
		t.Fatalf("peerless context keyed as %q", got)
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:5678":     "1.2.3.4",
		"1.2.3.4":          "1.2.3.4",
		"[2001:db8::1]:99": "2001:db8::1",
		// A bare IPv6 literal has colons but no brackets. Cutting at the last
		// colon would yield "2001:db8:" — and "::1" would become ":", putting
		// unrelated callers in one bucket.
		"2001:db8::1": "2001:db8::1",
		"::1":         "::1",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvictionRespectsMaxKeys(t *testing.T) {
	l := New(Config{PerMinute: 600, Burst: 1, MaxKeys: 10, TTL: time.Nanosecond})
	base := time.Now()
	for i := 0; i < 50; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		l.now = func() time.Time { return at }
		l.allowKey(netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)}).String())
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 10 {
		t.Fatalf("map grew past MaxKeys: %d buckets", n)
	}
}

func TestFromEnvDefaultsAreOn(t *testing.T) {
	// The default must be ENABLED. A limiter that ships off is absent
	// everywhere nobody thought to switch it on.
	cfg := FromEnv("TEST_UNSET_PREFIX")
	if cfg.PerMinute != DefaultPerMinute || cfg.Burst != DefaultBurst {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if New(cfg) == nil {
		t.Fatal("default config must build a live limiter")
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Fatal("default trusted proxies must be non-empty, or every caller shares one bucket")
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("X_RATELIMIT_PER_MINUTE", "5")
	t.Setenv("X_RATELIMIT_BURST", "2")
	t.Setenv("X_RATELIMIT_TRUSTED_PROXIES", "192.0.2.0/24")
	cfg := FromEnv("X")
	if cfg.PerMinute != 5 || cfg.Burst != 2 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if len(cfg.TrustedProxies) != 1 || cfg.TrustedProxies[0].String() != "192.0.2.0/24" {
		t.Fatalf("trusted proxies not applied: %+v", cfg.TrustedProxies)
	}
	t.Setenv("X_RATELIMIT_PER_MINUTE", "0")
	if New(FromEnv("X")) != nil {
		t.Fatal("PER_MINUTE=0 must disable")
	}
}
