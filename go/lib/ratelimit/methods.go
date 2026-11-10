package ratelimit

import (
	"log"
	"net/netip"
	"os"
	"strings"
)

// Method is one limited public method of a surface: its key and the
// threshold it declares (`(w17.rest_api)` endpoint `public_rate`, or an rpc
// surface's `public_limits[].rate`). A zero PerMinute or Burst takes the
// scope's value.
//
// Key is the method's name in the environment, `<SERVICE>_<METHOD>` in upper
// snake case (AUTH_SERVICE_SIGN_IN): the operator overrides the method alone
// through `<PREFIX>_<SCOPE>_RATELIMIT_<KEY>_PER_MINUTE` / `_BURST`.
type Method struct {
	Key       string
	PerMinute float64
	Burst     int
}

// Methods holds the limiters of one transport of a surface, one per limited
// method, each with its own bucket per client address. There is no shared
// bucket: a method spends only its own budget, so a page everyone loads never
// eats into a sign-in's, and a method that is not listed is not limited.
type Methods struct {
	scope   Config
	byKey   map[string]*Limiter
	streams *Limiter
}

// NewMethods resolves the limiter of each method, most specific first:
//
//  1. `<PREFIX>_<SCOPE>_RATELIMIT_<KEY>_PER_MINUTE` / `_BURST`: the
//     operator's value for this method alone (<= 0 switches it off);
//  2. the scope switched off (`<PREFIX>_<SCOPE>_RATELIMIT_PER_MINUTE`, or the
//     transport's, <= 0): every method is off;
//  3. the threshold the method declares;
//  4. the scope's values ([FromEnvScope]): the default of a method that
//     declares none.
//
// Trusted proxies, the key TTL and the key ceiling are the scope's for every
// method. The stream cap ([Methods.Streams]) is the scope's MAX_STREAMS and is
// independent of every rate: switching the scope or the transport off leaves
// it in force.
func NewMethods(prefix, scope, transport string, methods ...Method) *Methods {
	sc := FromEnvScope(prefix, scope, transport)
	m := &Methods{scope: sc, byKey: make(map[string]*Limiter, len(methods)), streams: New(sc)}
	for _, meth := range methods {
		m.byKey[meth.Key] = New(methodConfig(sc, prefix+"_"+scope+"_RATELIMIT_"+meth.Key, meth))
	}
	return m
}

// methodConfig is one method's config in the order NewMethods documents.
// It carries no stream cap: that is [Methods.Streams]' alone, so a method
// switched off has no limiter at all ([Methods.For] answers nil).
func methodConfig(sc Config, name string, meth Method) Config {
	cfg := sc
	cfg.MaxStreams = 0
	if meth.PerMinute > 0 && sc.PerMinute > 0 {
		cfg.PerMinute = meth.PerMinute
	}
	if meth.Burst > 0 {
		cfg.Burst = meth.Burst
	}
	if f, ok := parsePerMinute(strings.TrimSpace(os.Getenv(name + "_PER_MINUTE"))); ok {
		cfg.PerMinute = f
		if f <= 0 {
			if _, logged := offLogged.LoadOrStore(name, true); !logged {
				// name is the caller's env name, f a parsed number: nothing an
				// outside caller writes reaches this line.
				log.Printf("ratelimit: %s is OFF (%s_PER_MINUTE=%v) — this method is not limited per address", name, name, f) //nolint:gosec // G706: no outside input, see above
			}
		}
	}
	if n, ok := parseBurst(strings.TrimSpace(os.Getenv(name + "_BURST"))); ok {
		cfg.Burst = n
	}
	return cfg
}

// For is the limiter of the method keyed key: nil — which admits everything
// — for a method that was not listed or is switched off.
func (m *Methods) For(key string) *Limiter {
	if m == nil {
		return nil
	}
	return m.byKey[key]
}

// Streams is the limiter whose [Limiter.OpenStream] caps the streams one
// client holds open across every public stream of the transport
// ([Config.MaxStreams], off unless set). It spends no method's tokens, and
// the cap holds whether or not the scope's per-minute limit is on ([New]).
func (m *Methods) Streams() *Limiter {
	if m == nil {
		return nil
	}
	return m.streams
}

// TrustedProxies are the scope's: the proxies whose X-Forwarded-For is
// believed, read even when every limiter is off.
func (m *Methods) TrustedProxies() []netip.Prefix {
	if m == nil {
		return nil
	}
	return m.scope.TrustedProxies
}
