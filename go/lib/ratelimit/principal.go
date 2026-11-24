package ratelimit

import (
	"log"
	"math"
	"os"
	"strings"
	"time"
)

// DefaultPrincipalBurst is the burst of a per-principal limit the deployment
// switches on (`<PREFIX>_PRINCIPAL_RATELIMIT_PER_MINUTE`) for a surface that
// declares no `principal_rate`.
const DefaultPrincipalBurst = 60

// NewPrincipal builds the per-principal limit of one transport of a surface:
// one token bucket per AUTHENTICATED caller (the auth method's user_id), for
// the whole surface. It is the authenticated twin of the per-address limit
// on the methods that take no credential ([NewMethods]): that bounds
// guesses, this bounds what one token holder may ask of the backend.
//
// perMinute and burst are what the surface declares (`principal_rate`);
// zero means it declares none. Resolved most specific first:
//
//  1. `<PREFIX>_PRINCIPAL_RATELIMIT_<TRANSPORT>_PER_MINUTE` / `_BURST`;
//  2. `<PREFIX>_PRINCIPAL_RATELIMIT_PER_MINUTE` / `_BURST`;
//  3. the declared rate.
//
// `<= 0` switches it off, a positive value switches it on — on a surface that
// declares nothing too, at that rate and [DefaultPrincipalBurst]. Nothing
// declared and nothing set is OFF, unlike the per-address limit: a principal
// limit is a statement about what a caller is entitled to, which only the
// project or the deployment can make. The result is nil (admits everything)
// when off.
func NewPrincipal(prefix, transport string, perMinute float64, burst int) *Limiter {
	base := prefix + "_PRINCIPAL_RATELIMIT"
	cfg := Config{PerMinute: perMinute, Burst: burst, TTL: DefaultTTL, MaxKeys: DefaultMaxKeys}
	declared := perMinute > 0
	from := ""
	for _, name := range []string{base, base + "_" + transport} {
		if transport == "" && name != base {
			continue
		}
		if f, ok := parsePerMinute(strings.TrimSpace(os.Getenv(name + "_PER_MINUTE"))); ok {
			cfg.PerMinute, from = f, name+"_PER_MINUTE"
		}
		if n, ok := parseBurst(strings.TrimSpace(os.Getenv(name + "_BURST"))); ok {
			cfg.Burst = n
		}
	}
	if cfg.PerMinute <= 0 {
		if declared {
			name := base
			if transport != "" {
				name += "_" + transport
			}
			if _, logged := offLogged.LoadOrStore(name, true); !logged {
				// name and from are env names, PerMinute a parsed number:
				// nothing an outside caller writes reaches this line.
				log.Printf("ratelimit: %s is OFF (%s=%v) — authenticated callers are not limited per principal", name, from, cfg.PerMinute) //nolint:gosec // G706: no outside input, see above
			}
		}
		return nil
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultPrincipalBurst
	}
	return New(cfg)
}

// AllowPrincipal reports whether the authenticated caller principal may make
// a call costing cost tokens, spending them when it may. When it may not,
// retryAfter is how long until the bucket holds cost tokens again.
//
// A nil limiter, an empty principal (a caller the auth method answered
// without an id) and a cost <= 0 (an exempt method) are admitted without
// spending. A cost above the burst could never be paid; it is charged as the
// whole burst instead, and said once, so a deployment that lowered the burst
// below a method's declared cost slows that method rather than refusing it
// for ever.
func (l *Limiter) AllowPrincipal(principal string, cost int) (ok bool, retryAfter time.Duration) {
	if l.rateOff() || principal == "" || cost <= 0 {
		return true, 0
	}
	if cost > l.cfg.Burst {
		if _, logged := offLogged.LoadOrStore("principal-cost-over-burst", true); !logged {
			log.Printf("ratelimit: a method costs %d tokens, above the per-principal burst %d — it is charged the burst", cost, l.cfg.Burst)
		}
		cost = l.cfg.Burst
	}
	return l.bucketFor("p:"+principal).takeN(l.now(), l.perSec, l.cfg.Burst, float64(cost))
}

// takeN spends n tokens if they are there, atomically; otherwise it reports
// how long until they will be.
func (b *bucket) takeN(now time.Time, perSec float64, burst int, n float64) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now, perSec, burst)
	if b.tokens < n {
		wait := (n - b.tokens) / perSec
		return false, time.Duration(math.Ceil(wait)) * time.Second
	}
	b.tokens -= n
	return true, 0
}
