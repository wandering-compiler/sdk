package eventlog

import (
	"log"
	"time"
)

// ConsentCacheCeiling bounds how long a gateway answers from a cached auth
// verdict when its records carry the caller's marketing consent: a caller
// who withdraws it shows as withdrawn in the records within this time.
const ConsentCacheCeiling = 5 * time.Minute

// CapAuthCacheTTL is the auth cache TTL of a gateway whose records carry
// the marketing consent: ttl, or [ConsentCacheCeiling] when ttl is longer
// (logged, since the operator asked for more). 0 (the cache off) stays 0.
func CapAuthCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= ConsentCacheCeiling {
		return ttl
	}
	log.Printf("eventlog: auth cache TTL %s capped at %s: this gateway's request records carry the caller's marketing consent, and a withdrawal must show in them within %s", ttl, ConsentCacheCeiling, ConsentCacheCeiling)
	return ConsentCacheCeiling
}
