// Package ratelimit bounds how often one caller may reach a method that
// takes no credential.
//
// It exists for a specific hole. A gateway's authenticated methods are
// already gated by the bearer resolver — an attacker without a token gets
// nowhere and the cost of trying is one rejected call. The methods flagged
// ExcludeAuth are different: sign-in and token-mint MUST answer an
// unauthenticated caller, so they are the one place where guessing is free
// and repeatable. Nothing else in the generated stack bounds that. The REST
// transport's nginx config carries a rate-limit block, but the rpc transport
// has no nginx in front of it, and a TLS terminator does not help either:
// gRPC multiplexes many calls over ONE connection, so anything counting
// connections barely counts calls.
//
// Deliberately NOT a global cap. One bucket for the whole surface means one
// attacker starves every real user, which converts a credential-stuffing
// attempt into an outage. Buckets are per client address.
package ratelimit

import (
	"context"
	"log"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Defaults. Generous on purpose: this guards a human action (signing in),
// where even a slow typist stays two orders of magnitude below the limit,
// while credential stuffing needs thousands of attempts to be worth running.
// A surface whose public methods are genuinely high-traffic raises them
// through the environment rather than being designed around here.
const (
	DefaultPerMinute = 30
	DefaultBurst     = 10
	// How long an idle bucket survives. Long enough that a client pausing
	// between attempts keeps its history, short enough that the map tracks
	// current traffic rather than everyone who ever connected.
	DefaultTTL = 10 * time.Minute
	// Hard ceiling on tracked keys, so a source-address flood cannot turn
	// the limiter itself into the memory exhaustion it is there to prevent.
	// Reaching it evicts the least recently seen.
	DefaultMaxKeys = 10000
)

// Config is the resolved limiter configuration. PerMinute <= 0 disables the
// limiter entirely.
type Config struct {
	PerMinute float64
	Burst     int
	TTL       time.Duration
	MaxKeys   int

	// TrustedProxies are the peers whose X-Forwarded-For may be believed.
	// Empty means believe nobody, and every caller then buckets under the
	// address of whatever terminates TLS in front — which is one bucket for
	// the world, i.e. the global cap this package exists not to be.
	TrustedProxies []netip.Prefix
}

// FromEnv reads `<PREFIX>_RATELIMIT_*` and returns the resolved config.
//
//	<PREFIX>_RATELIMIT_PER_MINUTE      float, <=0 disables      (default 30)
//	<PREFIX>_RATELIMIT_BURST           int                      (default 10)
//	<PREFIX>_RATELIMIT_TRUSTED_PROXIES comma-separated CIDRs    (default private + loopback)
//
// Unset means ON at the defaults, not off. A protection that must be
// switched on is one that is off everywhere it was never thought about, and
// the surfaces this guards are exactly the ones nobody revisits.
func FromEnv(prefix string) Config {
	cfg := Config{
		PerMinute: DefaultPerMinute,
		Burst:     DefaultBurst,
		TTL:       DefaultTTL,
		MaxKeys:   DefaultMaxKeys,
	}
	if v := strings.TrimSpace(os.Getenv(prefix + "_RATELIMIT_PER_MINUTE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.PerMinute = f
		}
	}
	if v := strings.TrimSpace(os.Getenv(prefix + "_RATELIMIT_BURST")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Burst = n
		}
	}
	raw := strings.TrimSpace(os.Getenv(prefix + "_RATELIMIT_TRUSTED_PROXIES"))
	if raw == "" {
		// The gateway is reached through a terminator on the same container
		// network; these are the ranges that terminator can occupy. A public
		// address is never in here, so a direct caller can never talk its way
		// into another bucket by sending a header.
		raw = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	}
	for _, p := range strings.Split(raw, ",") {
		if pfx, err := netip.ParsePrefix(strings.TrimSpace(p)); err == nil {
			cfg.TrustedProxies = append(cfg.TrustedProxies, pfx)
		}
	}
	return cfg
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// Limiter hands out one token bucket per client address.
//
// A nil *Limiter allows everything, so a caller that decided not to build one
// needs no branch at the call site.
type Limiter struct {
	cfg     Config
	perSec  rate.Limit
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // swapped in tests

	// collapsedAt throttles the warning on [Limiter.warnCollapsed]: the
	// condition fires on every request for as long as the deployment is
	// wrong, and a line printed per request is a line nobody reads.
	collapsedAt time.Time
}

// New builds a Limiter, or returns nil when the config disables it.
func New(cfg Config) *Limiter {
	if cfg.PerMinute <= 0 {
		return nil
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultBurst
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = DefaultMaxKeys
	}
	return &Limiter{
		cfg:     cfg,
		perSec:  rate.Limit(cfg.PerMinute / 60.0),
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Allow reports whether the caller behind ctx may proceed.
func (l *Limiter) Allow(ctx context.Context) bool {
	if l == nil {
		return true
	}
	key, resolved := l.clientKey(ctx)
	if !resolved {
		l.warnCollapsed()
	}
	return l.allowKey(key)
}

// warnCollapsed reports the deployment fault described on [Limiter.clientKey]
// — once, and then at most once an hour, because it fires on every request
// while it lasts and a message that floods is a message nobody reads.
//
// It does NOT disable the limit. Turning the protection off on a
// misconfiguration would swap a visible outage for a silent hole, and this
// package's whole reason for defaulting ON is that the surfaces it guards are
// the ones nobody revisits. The operator gets told; the brake stays on.
func (l *Limiter) warnCollapsed() {
	now := l.now()
	l.mu.Lock()
	if !l.collapsedAt.IsZero() && now.Sub(l.collapsedAt) < time.Hour {
		l.mu.Unlock()
		return
	}
	l.collapsedAt = now
	l.mu.Unlock()

	log.Printf("ratelimit: the peer is a trusted proxy but forwarded no client address — " +
		"every caller is now sharing ONE bucket, so any single client can exhaust it and " +
		"lock everyone else out of the methods this limit protects. The real address is not " +
		"reaching this process: an L4/TCP load balancer forwards no X-Forwarded-For. " +
		"Terminate HTTP at the proxy (so it appends one), or enable PROXY protocol.")
}

func (l *Limiter) allowKey(key string) bool {
	now := l.now()

	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		// A fresh bucket starts FULL, so the limit never penalises a caller's
		// first request — it bounds sustained rate, not arrival.
		b = &bucket{lim: rate.NewLimiter(l.perSec, l.cfg.Burst)}
		l.buckets[key] = b
		if len(l.buckets) > l.cfg.MaxKeys {
			l.evictLocked(now)
		}
	}
	b.seen = now
	lim := b.lim
	l.mu.Unlock()

	// Outside the lock: rate.Limiter has its own.
	return lim.Allow()
}

// evictLocked drops idle buckets, then oldest-first if that was not enough.
// Caller holds l.mu.
func (l *Limiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.seen) > l.cfg.TTL {
			delete(l.buckets, k)
		}
	}
	for len(l.buckets) > l.cfg.MaxKeys {
		var oldestKey string
		var oldest time.Time
		for k, b := range l.buckets {
			if oldestKey == "" || b.seen.Before(oldest) {
				oldestKey, oldest = k, b.seen
			}
		}
		if oldestKey == "" {
			return
		}
		delete(l.buckets, oldestKey)
	}
}

// ClientKey resolves the address to bucket ctx's caller under.
//
// The immediate peer is a TLS terminator, not the client, so its address
// alone would put everyone in one bucket. X-Forwarded-For carries the real
// one — but a header is client-supplied, so believing it unconditionally
// lets any caller pick its own bucket and the limit stops meaning anything.
//
// So: believe the header only when the peer is a configured proxy, and read
// it RIGHT to LEFT, taking the first address that is not itself a trusted
// proxy. Left-to-right is the tempting version and it is the broken one —
// the leftmost entry is whatever the client sent before any proxy appended
// to it, which is to say whatever the client made up.
func (l *Limiter) ClientKey(ctx context.Context) string {
	key, _ := l.clientKey(ctx)
	return key
}

// clientKey is ClientKey plus the answer to "did we actually resolve a
// caller, or is this everyone at once".
//
// The second return is false in exactly one situation: the peer IS a trusted
// terminator — so a forwarded address is expected — and none arrived. Then
// every caller on the surface buckets under the terminator's own address,
// the per-client limit becomes one global cap, and any single client can
// drain it and refuse everybody else's SignIn. That is the inversion of the
// protection: the limiter stops being a brake on an attacker and becomes the
// attacker's lever (hosted review 2026-08-30, HOST-A-3, measured — two
// distinct clients keyed to one bucket and draining it refused a victim).
//
// It is a DEPLOYMENT fault, not a runtime state to paper over: an L4 (TCP)
// load balancer forwards no X-Forwarded-For, so the real address never
// reaches the process. The fix is upstream — terminate HTTP at the proxy, or
// enable PROXY protocol — which is why this reports rather than guesses.
func (l *Limiter) clientKey(ctx context.Context) (string, bool) {
	peerAddr := peerIP(ctx)

	if !l.trusted(peerAddr) {
		// The peer IS the client (no terminator in front, or an untrusted
		// one whose header we correctly refuse to believe).
		return addrKey(peerAddr, ctx), true
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return addrKey(peerAddr, ctx), false
	}
	var hops []string
	for _, v := range md.Get("x-forwarded-for") {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(stripPort(hops[i]))
		if err != nil {
			continue
		}
		if !l.trusted(ip) {
			return bucketForAddr(ip), true
		}
	}
	// A trusted peer that forwarded nothing usable — the collapse.
	return addrKey(peerAddr, ctx), false
}

func (l *Limiter) trusted(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, p := range l.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// addrKey is the fallback bucket name. An unparseable peer buckets under a
// single shared key rather than under "" per call: a caller whose address we
// cannot read must not get an unmetered bucket each time.
func addrKey(ip netip.Addr, _ context.Context) string {
	if ip.IsValid() {
		return bucketForAddr(ip)
	}
	return "unknown"
}

// bucketForAddr collapses an address to the unit a limit should actually
// count: one IPv4 address, or one IPv6 **/64**.
//
// Keying the full /128 measured as no protection at all — 1000 attempts from
// a single /64 produced 0 refusals, while 1000 from one IPv4 produced 990.
// Every ordinary IPv6 assignment hands out a /64 or shorter, so "one address"
// is not a unit of anything on that side of the wire: an attacker rotates
// through 2^64 free buckets while a legitimate IPv4 caller is metered
// normally. The limiter was strict against the callers it should not have
// been strict about, and absent against the one it existed for
// (hosted review 2026-08-30, HOST-A-4).
//
// /64 is the smallest block an operator is reliably assigned, so it is the
// smallest unit that cannot be widened for free. Going shorter (/48) would
// meter unrelated customers of one ISP together.
func bucketForAddr(ip netip.Addr) string {
	ip = ip.Unmap()
	if ip.Is6() {
		if pfx, err := ip.Prefix(64); err == nil {
			return pfx.String()
		}
	}
	return ip.String()
}

func peerIP(ctx context.Context) netip.Addr {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(stripPort(p.Addr.String()))
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

// stripPort removes a trailing :port, including from a bracketed IPv6
// host:port. A bare IPv6 literal has colons but no brackets, so it must not
// be cut at the last colon — that would turn ::1 into :: and bucket every
// loopback caller together with anything else that truncates to it.
func stripPort(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if i := strings.LastIndex(s, "]"); i > 0 {
			return s[1:i]
		}
		return s
	}
	if strings.Count(s, ":") == 1 {
		if i := strings.LastIndex(s, ":"); i > 0 {
			return s[:i]
		}
	}
	return s
}
