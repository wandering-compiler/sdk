// Package ratelimit bounds how often one caller may reach a method that
// takes no credential.
//
// It exists for a specific hole. A gateway's authenticated methods are
// already gated by the bearer resolver — an attacker without a token gets
// nowhere and the cost of trying is one rejected call. The methods flagged
// ExcludeAuth are different: sign-in and token-mint MUST answer an
// unauthenticated caller, so they are the one place where guessing is free
// and repeatable. Nothing else in the generated stack bounds that: the edge
// in front proxies whole hosts and counts nothing per endpoint, and counting
// connections would not help either — gRPC multiplexes many calls over ONE
// connection. So every transport (rpc, REST, the admin's sign-in) limits its
// public methods in-process, through this package.
//
// Deliberately NOT a global cap. One bucket for the whole surface means one
// attacker starves every real user, which converts a credential-stuffing
// attempt into an outage. Buckets are per client address.
//
// # Behind a proxy
//
// By default the client address is the address of the connection, and
// X-Forwarded-For is ignored. A process reached through an HTTP terminator
// (a reverse proxy, an ingress, a cloud load balancer working at HTTP) must
// list that terminator in `<...>_RATELIMIT_TRUSTED_PROXIES`, or every client
// shares the terminator's bucket — the limiter warns when it sees a forwarded
// header from a private-range peer it does not trust. List only hops that
// append the address they received the request from; a hop that forwards
// the client's header untouched must not be trusted, because the client
// would then choose its own bucket. See [FromEnv] and [Config.TrustedProxies].
package ratelimit

import (
	"context"
	"log"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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

// TrustPrivateRanges is the value of `<...>_RATELIMIT_TRUSTED_PROXIES` that
// trusts the loopback ranges and the RFC 1918 private IPv4 ranges
// (127.0.0.0/8, ::1/128, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). It may be
// combined with CIDRs: `private,192.0.2.0/24`.
//
// It is opt-in because it is only correct when EVERY hop that can reach the
// process from a private address appends the client's address to
// X-Forwarded-For. A hop that forwards the connection without appending (an
// L4/TCP load balancer, a container runtime's port proxy, a sidecar) passes
// the client's own header through unchanged, and the limiter would then key
// each request by whatever address the client wrote there.
const TrustPrivateRanges = "private"

// privateRanges is what [TrustPrivateRanges] expands to.
var privateRanges = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// Config is the resolved limiter configuration. PerMinute <= 0 disables the
// limiter entirely.
type Config struct {
	PerMinute float64
	Burst     int
	TTL       time.Duration
	MaxKeys   int

	// TrustedProxies are the peers whose X-Forwarded-For may be believed.
	// List exactly the hops that terminate HTTP in front of this process and
	// APPEND the address they received the request from; nothing else.
	//
	// Empty (the default) means believe nobody: every caller is keyed by the
	// address of its own connection. Behind a terminator that is the
	// terminator's address, i.e. one bucket for every client — the process
	// logs a warning when it sees a forwarded header from a private-range
	// peer it does not trust, naming the variable to set.
	TrustedProxies []netip.Prefix

	// MaxStreams caps the streams one client may hold open at once on the
	// methods this limiter guards (see [Limiter.OpenStream]). Zero, the
	// default, means no cap: an open stream costs a token when it opens and
	// nothing while it stays open. Opt in where the load of many concurrent
	// streams per address is not one the deployment plans for; too many
	// streams is an overload, not a breach, so nothing is capped unasked.
	MaxStreams int
}

// FromEnv reads `<PREFIX>_RATELIMIT_*` and returns the resolved config.
//
//	<PREFIX>_RATELIMIT_PER_MINUTE      float, <=0 disables      (default 30)
//	<PREFIX>_RATELIMIT_BURST           int                      (default 10)
//	<PREFIX>_RATELIMIT_TRUSTED_PROXIES comma-separated CIDRs    (default none)
//	<PREFIX>_RATELIMIT_MAX_STREAMS     int, open streams per client (default 0: no cap)
//
// TRUSTED_PROXIES lists the proxies whose X-Forwarded-For is believed; see
// [Config.TrustedProxies]. The keyword [TrustPrivateRanges] (`private`)
// trusts every loopback and private range, as earlier versions did by
// default; use it only when every hop in those ranges appends its own entry.
// `none` states the default explicitly.
//
// Unset means ON at the defaults, not off. A protection that must be
// switched on is one that is off everywhere it was never thought about, and
// the surfaces this guards are exactly the ones nobody revisits.
func FromEnv(prefix string) Config {
	cfg, _ := fromEnvNamed(prefix+"_RATELIMIT", "")
	return cfg
}

// fromEnvNamed resolves `<base>_PER_MINUTE`, `_BURST`, `_TRUSTED_PROXIES` and
// `_MAX_STREAMS`.
// A variable unset under base is read under legacy (when given), with a
// warning that names the new spelling. perMinuteFrom is the variable that
// set PerMinute, empty when the default stands.
func fromEnvNamed(base, legacy string) (cfg Config, perMinuteFrom string) {
	cfg = Config{
		PerMinute: DefaultPerMinute,
		Burst:     DefaultBurst,
		TTL:       DefaultTTL,
		MaxKeys:   DefaultMaxKeys,
	}
	lookup := func(suffix string) (string, string) {
		if v := strings.TrimSpace(os.Getenv(base + suffix)); v != "" {
			return v, base + suffix
		}
		if legacy == "" {
			return "", ""
		}
		v := strings.TrimSpace(os.Getenv(legacy + suffix))
		if v == "" {
			return "", ""
		}
		if _, warned := legacyWarned.LoadOrStore(legacy+suffix, true); !warned {
			log.Printf("ratelimit: %s is the old name of %s — it still works; rename it", legacy+suffix, base+suffix)
		}
		return v, legacy + suffix
	}
	if v, from := lookup("_PER_MINUTE"); v != "" {
		if f, ok := parsePerMinute(v); ok {
			cfg.PerMinute, perMinuteFrom = f, from
		}
	}
	if v, _ := lookup("_BURST"); v != "" {
		if n, ok := parseBurst(v); ok {
			cfg.Burst = n
		}
	}
	raw, from := lookup("_TRUSTED_PROXIES")
	cfg.TrustedProxies = parseTrustedProxies(raw, from)
	if v, from := lookup("_MAX_STREAMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MaxStreams = n
		} else if _, warned := legacyWarned.LoadOrStore("bad:"+from+"="+v, true); !warned {
			log.Printf("ratelimit: %s: %q is not a whole number >= 0 — no stream cap", from, v)
		}
	}
	return cfg, perMinuteFrom
}

// parseTrustedProxies reads a TRUSTED_PROXIES value: comma-separated CIDRs or
// bare addresses, plus the keywords `private` and `none`. An entry that is
// none of those is reported and skipped — the limiter then trusts LESS than
// intended, which fails towards one shared bucket rather than towards
// believing a forged header.
func parseTrustedProxies(raw, from string) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		switch strings.ToLower(p) {
		case "", "none":
			continue
		case TrustPrivateRanges:
			for _, r := range privateRanges {
				out = append(out, netip.MustParsePrefix(r))
			}
			continue
		}
		if pfx, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pfx.Masked())
			continue
		}
		if a, err := netip.ParseAddr(p); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		if _, warned := legacyWarned.LoadOrStore("bad:"+from+"="+p, true); !warned {
			log.Printf("ratelimit: %s: %q is not a CIDR, an address, %q or \"none\" — ignored", from, p, TrustPrivateRanges)
		}
	}
	return out
}

// parsePerMinute and parseBurst are the one parse of each value, whichever
// variable it came from; ok is false for an empty or malformed value, which
// leaves the previous one standing.
func parsePerMinute(v string) (float64, bool) {
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

func parseBurst(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

// offLogged and legacyWarned hold what [FromEnvScope] has already reported,
// so each line is printed once: a surface builds more than one limiter from
// the same variables.
var offLogged, legacyWarned sync.Map

// FromEnvScope reads the limiter of one SCOPE of a surface — what it limits
// is in the variable's name:
//
//	<PREFIX>_<SCOPE>_RATELIMIT_PER_MINUTE      float, <=0 disables  (default 30)
//	<PREFIX>_<SCOPE>_RATELIMIT_BURST           int                  (default 10)
//	<PREFIX>_<SCOPE>_RATELIMIT_TRUSTED_PROXIES CIDRs                (default none; see [FromEnv])
//	<PREFIX>_<SCOPE>_RATELIMIT_MAX_STREAMS     int                  (default 0: no cap; see [Config.MaxStreams])
//
// The gateway's scope is PUBLIC, the methods that take no credential (a
// sign-in, a registration, a reset request, a code check) and nothing else:
// an authenticated call is not limited here. The admin's is LOGIN. A bare
// `<PREFIX>_RATELIMIT_*`, the name these had before, is still read when the
// scoped one is unset, with a warning.
//
// transport, when not empty, narrows it to one transport of a surface that
// serves several (REST and the gRPC rpc transport share a gateway):
// `<PREFIX>_<SCOPE>_RATELIMIT_<TRANSPORT>_PER_MINUTE` / `_BURST` override the
// scope's values for that transport alone, so one can be switched off — a
// conscious decision, made in the environment — while the other stays
// limited. Unset means ON at the defaults. A limiter that ends up off says
// so once, at start, naming the variable that switched it off.
func FromEnvScope(prefix, scope, transport string) Config {
	base := prefix + "_" + scope + "_RATELIMIT"
	cfg, from := fromEnvNamed(base, prefix+"_RATELIMIT")
	name := base
	if transport != "" {
		name = base + "_" + transport
		if f, ok := parsePerMinute(strings.TrimSpace(os.Getenv(name + "_PER_MINUTE"))); ok {
			cfg.PerMinute, from = f, name+"_PER_MINUTE"
		}
		if n, ok := parseBurst(strings.TrimSpace(os.Getenv(name + "_BURST"))); ok {
			cfg.Burst = n
		}
	}
	if cfg.PerMinute <= 0 {
		if _, logged := offLogged.LoadOrStore(name, true); !logged {
			log.Printf("ratelimit: %s is OFF (%s=%v) — %s", name, from, cfg.PerMinute, scopeMeaning(scope))
		}
	}
	return cfg
}

// scopeMeaning says, in the OFF line, what is now unlimited.
func scopeMeaning(scope string) string {
	switch scope {
	case "PUBLIC":
		return "the methods that take no credential (sign-in, registration, reset, codes) are not limited per address"
	case "LOGIN":
		return "the sign-in is not limited per address"
	}
	return "nothing is limited per address"
}

// bucket is one client's token bucket. Its tokens are guarded by its own
// mutex so that callers keyed differently never contend; key, seen and the
// recency links belong to the Limiter and are guarded by Limiter.mu.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time

	key   string
	seen  time.Time
	newer *bucket // towards the most recently seen
	older *bucket // towards the least recently seen
}

// refillLocked advances the bucket to now. Caller holds b.mu.
func (b *bucket) refillLocked(now time.Time, perSec float64, burst int) {
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * perSec
		b.last = now
	}
	if max := float64(burst); b.tokens > max {
		b.tokens = max
	}
}

// take spends one token if one is there, atomically: the check and the
// spend cannot be separated by another caller.
func (b *bucket) take(now time.Time, perSec float64, burst int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now, perSec, burst)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// give returns one token, never past the burst.
func (b *bucket) give(now time.Time, perSec float64, burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now, perSec, burst)
	b.tokens++
	if max := float64(burst); b.tokens > max {
		b.tokens = max
	}
}

// Limiter hands out one token bucket per client address.
//
// A nil *Limiter allows everything, so a caller that decided not to build one
// needs no branch at the call site.
type Limiter struct {
	cfg     Config
	perSec  float64
	mu      sync.Mutex
	buckets map[string]*bucket
	// newest and oldest are the ends of the recency list every bucket is on,
	// so eviction takes the least recently seen key without looking at any
	// other.
	newest, oldest *bucket
	now            func() time.Time // swapped in tests

	// collapsedAt and untrustedAt throttle the deployment warnings: each
	// condition fires on every request for as long as the deployment is
	// wrong, and a line printed per request is a line nobody reads.
	collapsedAt time.Time
	untrustedAt time.Time

	// streams counts the open streams per client key while MaxStreams caps
	// them. Apart from the buckets: a bucket may be evicted, an open stream
	// must stay counted until it closes.
	streamsMu sync.Mutex
	streams   map[string]int
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
		perSec:  cfg.PerMinute / 60.0,
		buckets: make(map[string]*bucket),
		streams: make(map[string]int),
		now:     time.Now,
	}
}

// OpenStream counts one more stream open for the client of r against
// [Config.MaxStreams]. It reports false, counting nothing, when the client
// already holds that many; otherwise release must be called once the stream
// ends (later calls do nothing). It spends no token. A nil limiter, or one
// without a cap, admits every stream.
func (l *Limiter) OpenStream(r *http.Request) (ok bool, release func()) {
	if l == nil || l.cfg.MaxStreams <= 0 {
		return true, func() {}
	}
	key := l.resolve(l.requestKey(r))
	l.streamsMu.Lock()
	defer l.streamsMu.Unlock()
	if l.streams[key] >= l.cfg.MaxStreams {
		return false, func() {}
	}
	l.streams[key]++
	var once sync.Once
	return true, func() {
		once.Do(func() {
			l.streamsMu.Lock()
			defer l.streamsMu.Unlock()
			if l.streams[key]--; l.streams[key] <= 0 {
				delete(l.streams, key)
			}
		})
	}
}

// Allow reports whether the caller behind ctx may proceed, spending one
// token when it may.
func (l *Limiter) Allow(ctx context.Context) bool {
	if l == nil {
		return true
	}
	return l.allowKey(l.resolve(l.clientKey(ctx)))
}

// resolve reports what keyFor found wrong with the deployment, if anything,
// and passes the key through.
func (l *Limiter) resolve(key string, st keyState) string {
	switch st {
	case keyCollapsed:
		l.warnCollapsed()
	case keyUntrustedForwarder:
		l.warnUntrustedForwarder()
	}
	return key
}

// throttled reports whether a warning last printed at *at may print again,
// and if so records now. It is printed once, then at most once an hour.
func (l *Limiter) throttled(at *time.Time) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !at.IsZero() && now.Sub(*at) < time.Hour {
		return true
	}
	*at = now
	return false
}

// warnCollapsed reports the deployment fault described on [Limiter.clientKey].
//
// It does NOT disable the limit. Turning the protection off on a
// misconfiguration would swap a visible outage for a silent hole, and this
// package's whole reason for defaulting ON is that the surfaces it guards are
// the ones nobody revisits. The operator gets told; the brake stays on.
func (l *Limiter) warnCollapsed() {
	if l.throttled(&l.collapsedAt) {
		return
	}
	log.Printf("ratelimit: the peer is a trusted proxy but forwarded no client address — " +
		"every caller is now sharing ONE bucket, so any single client can exhaust it and " +
		"lock everyone else out of the methods this limit protects. The real address is not " +
		"reaching this process: an L4/TCP load balancer forwards no X-Forwarded-For. " +
		"Terminate HTTP at the proxy (so it appends one), or enable PROXY protocol.")
}

// warnUntrustedForwarder reports a private-range peer that sent
// X-Forwarded-For without being a configured proxy. That is what an
// unconfigured deployment behind a terminator looks like: the header is
// ignored, as it must be, and every client shares the terminator's bucket.
func (l *Limiter) warnUntrustedForwarder() {
	if l.throttled(&l.untrustedAt) {
		return
	}
	log.Printf("ratelimit: a private-range peer sent X-Forwarded-For but is not a trusted proxy, " +
		"so the header is ignored and every caller behind it shares ONE bucket. If that peer is " +
		"your HTTP terminator and it appends the client address, list it in " +
		"the limiter's _RATELIMIT_TRUSTED_PROXIES variable (CIDRs, or \"private\" when every private-range hop appends).")
}

// Reserve admits one attempt at a CREDENTIAL endpoint and spends its token
// up front. done settles the attempt once its outcome is known: done(true)
// (it failed) keeps the token spent, done(false) (it succeeded) gives it back.
// ok=false means the caller's bucket is empty; nothing was spent and done is
// a no-op.
//
// Rate-limiting a sign-in is about bounding guesses, not sign-ins. An
// attacker needs thousands of WRONG attempts to be worth running, while a
// legitimate caller — a script re-authenticating per request, an office
// behind one NAT address — can make many RIGHT ones in a burst and is not
// the thing being defended against. So only failures stay charged.
//
// The token is taken BEFORE the attempt runs, not after it fails: attempts in
// flight at the same time each hold a token of their own, so the number of
// attempts that can be outstanding — and therefore the number of guesses a
// caller gets — never exceeds what the bucket holds, however many it sends
// at once.
//
// done is idempotent, safe from a defer, and only the first call counts. A
// done that is never called leaves the token spent. A nil Limiter admits and
// returns a no-op done.
func (l *Limiter) Reserve(ctx context.Context) (ok bool, done func(failed bool)) {
	if l == nil {
		return true, func(bool) {}
	}
	return l.reserve(l.resolve(l.clientKey(ctx)))
}

// ReserveRequest is [Limiter.Reserve] for an HTTP request (see
// [Limiter.AllowRequest] for how the caller is resolved).
func (l *Limiter) ReserveRequest(r *http.Request) (ok bool, done func(failed bool)) {
	if l == nil {
		return true, func(bool) {}
	}
	return l.reserve(l.resolve(l.requestKey(r)))
}

func (l *Limiter) reserve(key string) (bool, func(failed bool)) {
	b := l.bucketFor(key)
	if !b.take(l.now(), l.perSec, l.cfg.Burst) {
		return false, func(bool) {}
	}
	var once sync.Once
	return true, func(failed bool) {
		once.Do(func() {
			if !failed {
				b.give(l.now(), l.perSec, l.cfg.Burst)
			}
		})
	}
}

// AllowChargeable is the older form of [Limiter.Reserve], for callers that
// signal a failure but not a success: it admits an attempt and returns the
// function to call when the attempt failed.
//
// The token is spent at admission, for the reason Reserve gives — admitting
// on a peek at the bucket and charging after the failure let every attempt
// in flight through at once. Because a success is never signalled here, the
// token is given back when ctx ends without charge having been called: for a
// gRPC handler's or an HTTP request's context that is when the call has
// completed. A context that is never cancelled never gives its token back, so
// every attempt stays charged; prefer Reserve, which settles explicitly.
//
// charge is idempotent and safe to call from a defer. A charge that arrives
// after the context already gave the token back takes one again when the
// bucket has one. A nil Limiter admits and returns a no-op charge.
func (l *Limiter) AllowChargeable(ctx context.Context) (allowed bool, charge func()) {
	if l == nil {
		return true, func() {}
	}
	return l.chargeable(ctx, l.resolve(l.clientKey(ctx)))
}

// AllowChargeableRequest is [Limiter.AllowChargeable] for an HTTP request
// (see [Limiter.AllowRequest] for how the caller is resolved); the token is
// given back when r's context ends without a charge.
func (l *Limiter) AllowChargeableRequest(r *http.Request) (allowed bool, charge func()) {
	if l == nil {
		return true, func() {}
	}
	return l.chargeable(r.Context(), l.resolve(l.requestKey(r)))
}

func (l *Limiter) chargeable(ctx context.Context, key string) (bool, func()) {
	b := l.bucketFor(key)
	if !b.take(l.now(), l.perSec, l.cfg.Burst) {
		return false, func() {}
	}
	var (
		mu       sync.Mutex
		charged  bool
		refunded bool
	)
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !charged && !refunded {
			refunded = true
			b.give(l.now(), l.perSec, l.cfg.Burst)
		}
	})
	return true, func() {
		mu.Lock()
		defer mu.Unlock()
		if charged {
			return
		}
		charged = true
		stop()
		if refunded {
			b.take(l.now(), l.perSec, l.cfg.Burst)
		}
	}
}

func (l *Limiter) allowKey(key string) bool {
	return l.bucketFor(key).take(l.now(), l.perSec, l.cfg.Burst)
}

// bucketFor returns the per-key token bucket, creating it on first use.
func (l *Limiter) bucketFor(key string) *bucket {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if ok {
		l.unlinkLocked(b)
	} else {
		// A fresh bucket starts FULL, so the limit never penalises a caller's
		// first request — it bounds sustained rate, not arrival.
		b = &bucket{tokens: float64(l.cfg.Burst), last: now, key: key}
		l.buckets[key] = b
	}
	b.seen = now
	l.pushNewestLocked(b)
	if !ok {
		l.evictLocked(now)
	}
	return b
}

// evictSweep bounds how many idle buckets one new key retires beyond what
// the ceiling requires. Each bucket is retired at most once, so the work
// stays constant per request however many keys there are.
const evictSweep = 2

// evictLocked retires buckets from the least recently seen end: idle ones
// (a few per call, so the map shrinks back once a burst of clients is gone),
// then as many as the ceiling requires. The least recently seen bucket is
// the idle one if any is, so idle keys always go before live ones.
//
// This used to scan the whole map — once for idle keys, then once more per
// key it had to remove — under the mutex every request takes, so a stream of
// never-seen addresses cost two full scans each. It also stamped the new
// bucket's time only after evicting, so the newcomer looked oldest and was
// the one removed: at the ceiling, each request from a new address got a
// full bucket of its own. Caller holds l.mu.
func (l *Limiter) evictLocked(now time.Time) {
	for i := 0; i < evictSweep; i++ {
		b := l.oldest
		if b == nil || b == l.newest || now.Sub(b.seen) <= l.cfg.TTL {
			break
		}
		l.removeLocked(b)
	}
	for len(l.buckets) > l.cfg.MaxKeys && l.oldest != nil && l.oldest != l.newest {
		l.removeLocked(l.oldest)
	}
}

func (l *Limiter) removeLocked(b *bucket) {
	l.unlinkLocked(b)
	delete(l.buckets, b.key)
}

// unlinkLocked takes b off the recency list. Caller holds l.mu.
func (l *Limiter) unlinkLocked(b *bucket) {
	if b.newer != nil {
		b.newer.older = b.older
	} else if l.newest == b {
		l.newest = b.older
	}
	if b.older != nil {
		b.older.newer = b.newer
	} else if l.oldest == b {
		l.oldest = b.newer
	}
	b.newer, b.older = nil, nil
}

// pushNewestLocked puts an unlinked b at the most recently seen end. Caller
// holds l.mu.
func (l *Limiter) pushNewestLocked(b *bucket) {
	b.older = l.newest
	if l.newest != nil {
		l.newest.newer = b
	}
	l.newest = b
	if l.oldest == nil {
		l.oldest = b
	}
}

// keyState is what resolving a caller found about the deployment.
type keyState int

const (
	// keyResolved: the key is the caller.
	keyResolved keyState = iota
	// keyCollapsed: a trusted proxy forwarded no usable address, so the key
	// is the proxy's and every caller behind it shares it.
	keyCollapsed
	// keyUntrustedForwarder: a private-range peer that is not a configured
	// proxy sent X-Forwarded-For. The header was ignored and the peer is the
	// key — correct, but most likely a terminator nobody listed.
	keyUntrustedForwarder
)

// ClientKey resolves the address to bucket ctx's caller under.
//
// The immediate peer may be a TLS terminator rather than the client, and
// X-Forwarded-For then carries the client's address — but the header is
// client-supplied, so believing it unconditionally lets any caller pick its
// own bucket and the limit stops meaning anything.
//
// So: believe the header only when the peer is a configured proxy
// ([Config.TrustedProxies]), and read it RIGHT to LEFT, skipping addresses
// that are themselves trusted proxies, and take the first one that is not.
// That is the address the outermost trusted proxy received the request
// from. Left-to-right is the tempting version and it is the broken one —
// the leftmost entry is whatever the client sent before any proxy appended
// to it.
func (l *Limiter) ClientKey(ctx context.Context) string {
	key, _ := l.clientKey(ctx)
	return key
}

// clientKey is ClientKey plus what it found about the deployment.
//
// keyCollapsed means the peer IS a trusted terminator — so a forwarded
// address is expected — and none arrived. Then every caller on the surface
// buckets under the terminator's own address, the per-client limit becomes
// one global cap, and any single client can drain it and refuse everybody
// else. It is a DEPLOYMENT fault, not a runtime state to paper over: an L4
// (TCP) load balancer forwards no X-Forwarded-For, so the real address never
// reaches the process. The fix is upstream — terminate HTTP at the proxy, or
// enable PROXY protocol — which is why this reports rather than guesses.
func (l *Limiter) clientKey(ctx context.Context) (string, keyState) {
	var forwarded []string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		forwarded = md.Get("x-forwarded-for")
	}
	return l.keyFor(peerIP(ctx), forwarded)
}

// keyFor is the transport-free half of clientKey: the immediate peer and the
// X-Forwarded-For values it sent, from gRPC metadata or an HTTP header alike.
func (l *Limiter) keyFor(peerAddr netip.Addr, forwarded []string) (string, keyState) {
	if !l.trusted(peerAddr) {
		// The peer IS the client (no terminator in front, or one that is not
		// configured, whose header must not be believed).
		st := keyResolved
		if hasForwarded(forwarded) && isPrivate(peerAddr) {
			st = keyUntrustedForwarder
		}
		return addrKey(peerAddr), st
	}

	var hops []string
	for _, v := range forwarded {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(stripPort(hops[i]))
		if err != nil {
			// A hop a trusted proxy wrote but that is not an address (an
			// obfuscated identifier, "unknown"). Everything left of it is
			// unverified, so stop rather than skip past it.
			break
		}
		if !l.trusted(ip) {
			return bucketForAddr(ip), keyResolved
		}
	}
	// A trusted peer that forwarded nothing usable — the collapse.
	return addrKey(peerAddr), keyCollapsed
}

func hasForwarded(forwarded []string) bool {
	for _, v := range forwarded {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func isPrivate(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && (ip.IsLoopback() || ip.IsPrivate())
}

func (l *Limiter) trusted(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	// A dual-stack listener reports an IPv4 peer as ::ffff:a.b.c.d, which an
	// IPv4 prefix does not contain.
	ip = ip.Unmap()
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
func addrKey(ip netip.Addr) string {
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
// normally.
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

// AllowRequest is [Limiter.Allow] for an HTTP request: the caller is
// r.RemoteAddr, or — when that is a trusted proxy — the X-Forwarded-For hop
// it appended, read under the same right-to-left rule.
//
// The context-based Allow cannot serve HTTP. An http.Request's context
// carries neither a gRPC peer nor incoming metadata, so every request
// would resolve to the one fallback key and the per-client limit would be a
// single global bucket.
func (l *Limiter) AllowRequest(r *http.Request) bool {
	if l == nil {
		return true
	}
	return l.allowKey(l.resolve(l.requestKey(r)))
}

// RequestKey is [Limiter.ClientKey] for an HTTP request.
func (l *Limiter) RequestKey(r *http.Request) string {
	key, _ := l.requestKey(r)
	return key
}

func (l *Limiter) requestKey(r *http.Request) (string, keyState) {
	ip, err := netip.ParseAddr(stripPort(r.RemoteAddr))
	if err != nil {
		ip = netip.Addr{}
	}
	return l.keyFor(ip, r.Header.Values("X-Forwarded-For"))
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
