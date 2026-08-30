package ratelimit

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
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

// newTestCtx builds a context whose gRPC peer is addr, optionally behind a
// trusted proxy that forwarded xff.
func newTestCtx(peerAddr string, xff ...string) context.Context {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(peerAddr), Port: 40000},
	})
	if len(xff) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", strings.Join(xff, ", ")))
	}
	return ctx
}

// TestLimiter_IPv6BucketsByPrefixNotByAddress — an IPv6 /64 is one caller.
//
// Hosted review 2026-08-30, HOST-A-4, and these are the measured numbers the
// finding was filed on: 1000 attempts from one /64 produced ZERO refusals
// while 1000 from a single IPv4 produced 990. Every ordinary IPv6 assignment
// is a /64 or shorter, so keying the full /128 gave an attacker 2^64 free
// buckets — the limiter was absent exactly where credential stuffing comes
// from, and strict everywhere else.
func TestLimiter_IPv6BucketsByPrefixNotByAddress(t *testing.T) {
	const trusted = "10.0.0.1"
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	count := func(addrs func(i int) string) (blocked int) {
		l := New(Config{PerMinute: 30, Burst: 10, TTL: time.Minute, MaxKeys: 10000, TrustedProxies: proxies})
		for i := 0; i < 1000; i++ {
			if !l.Allow(newTestCtx(trusted, addrs(i))) {
				blocked++
			}
		}
		return blocked
	}

	// One /64, rotating the host part — what an attacker actually has.
	v6 := count(func(i int) string { return fmt.Sprintf("2001:db8:1:1::%x", i) })
	if v6 < 900 {
		t.Errorf("1000 attempts from ONE IPv6 /64 were blocked only %d times — an attacker rotating the host part of a single assigned block gets unmetered attempts", v6)
	}

	// A single IPv4 address: unchanged behaviour, the control case.
	v4 := count(func(int) string { return "203.0.113.7" })
	if v4 < 900 {
		t.Errorf("1000 attempts from one IPv4 address were blocked only %d times — the limit stopped working at all", v4)
	}

	// And distinct /64s must still get distinct buckets, or the fix would
	// have meant "meter the whole internet as one caller".
	l := New(Config{PerMinute: 30, Burst: 10, TTL: time.Minute, MaxKeys: 10000, TrustedProxies: proxies})
	for i := 0; i < 100; i++ {
		if !l.Allow(newTestCtx(trusted, fmt.Sprintf("2001:db8:%x::1", i))) {
			t.Fatalf("a caller from a distinct /64 (#%d) was refused — separate networks must get separate buckets", i)
		}
	}
}

// TestLimiter_WarnsWhenEveryCallerCollapsesIntoOneBucket — the control must
// not invert silently.
//
// Hosted review 2026-08-30, HOST-A-3, measured: behind a trusted terminator
// that forwards no address, two distinct external clients key to ONE bucket,
// and draining it refuses a victim's SignIn. That is the protection turned
// into the attack — any single client can lock the whole surface out of the
// methods the limiter exists to protect. An L4/TCP load balancer forwards no
// X-Forwarded-For, so this is a deployment the docs describe, not a
// hypothetical.
//
// The limiter cannot invent the missing address, so the requirement is that
// it SAYS SO rather than quietly becoming a global cap.
func TestLimiter_WarnsWhenEveryCallerCollapsesIntoOneBucket(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	l := New(Config{
		PerMinute:      30,
		Burst:          10,
		TTL:            time.Minute,
		MaxKeys:        100,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})

	// A trusted terminator that forwarded nothing — the L4 case.
	if !l.Allow(newTestCtx("10.0.0.1")) {
		t.Fatal("the first request must not be refused; the limit is still supposed to work")
	}
	if buf.Len() == 0 {
		t.Fatal("every caller now shares one bucket and nothing said so — the operator learns about it from an outage instead")
	}
	for _, want := range []string{"ONE bucket", "PROXY protocol"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the warning must name the condition and the fix; missing %q in: %s", want, buf.String())
		}
	}

	// It must not repeat per request — the condition holds for every call
	// while the deployment is wrong.
	before := buf.Len()
	for i := 0; i < 50; i++ {
		l.Allow(newTestCtx("10.0.0.1"))
	}
	if buf.Len() != before {
		t.Errorf("the warning repeated per request (%d → %d bytes); a message that floods is one nobody reads", before, buf.Len())
	}
}

// TestLimiter_NoWarningWhenTheClientIsResolvable — the other direction. A
// correctly-fronted deployment must stay silent, or the warning is noise and
// gets muted.
func TestLimiter_NoWarningWhenTheClientIsResolvable(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	l := New(Config{
		PerMinute:      30,
		Burst:          10,
		TTL:            time.Minute,
		MaxKeys:        100,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	// Behind a trusted proxy that DID forward the client.
	l.Allow(newTestCtx("10.0.0.1", "203.0.113.7"))
	// And with no proxy at all — the peer is the client.
	l.Allow(newTestCtx("203.0.113.8"))

	if buf.Len() != 0 {
		t.Errorf("a resolvable client produced a collapse warning: %s", buf.String())
	}
}
