// Package kvposture checks, at boot, whether the Redis a generated binary
// just connected to is configured in a way that can silently drop entities the
// declaration says it stores.
//
// T1-4 pass #12, B-F8. A `(w17.db)` model on a KV connection is an ENTITY —
// the same word the SQL tiers use — but nothing ever asked the server whether
// it was going to keep it. On an instance configured as a CACHE
// (`maxmemory-policy allkeys-*`) a stored entity is evictable: it disappears
// under memory pressure, at no particular time, with no error on either side.
//
// It WARNS and returns. It does not stop the boot, for two reasons: a detector
// that can fail a rollout becomes an outage of its own (the same call
// `sqlitecollate`'s stamp makes when it finds no stamp at all), and
// `allkeys-lru` is a legitimate deployment for a model that really is a cache.
// The warning names the policy and the connection so an operator can tell
// which of the two they have.
package kvposture

import (
	"context"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"
)

// WarnIfEvicting logs a warning when `client`'s eviction policy can evict keys
// that carry no TTL — i.e. when a stored entity is evictable.
//
// A failure to READ the policy is not a finding, and is logged at debug level
// only: managed Redis commonly restricts `CONFIG GET`, and a binary that
// complains on every boot against a locked-down provider teaches its operator
// to stop reading the log. The silence there is deliberate.
func WarnIfEvicting(ctx context.Context, connection string, client *redis.Client) {
	if client == nil {
		return
	}
	vals, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil || len(vals) == 0 {
		slog.Debug("kv posture: eviction policy not readable — skipping the check",
			"connection", connection, "err", err)
		return
	}
	policy := vals["maxmemory-policy"]
	if !EvictsUntaggedKeys(policy) {
		return
	}
	slog.Warn("kv posture: this Redis evicts keys that carry no TTL, so a stored entity can disappear under memory pressure",
		"connection", connection,
		"maxmemory_policy", strings.TrimSpace(strings.ToLower(policy)),
		"why", "an `allkeys-*` policy is a CACHE posture, and the models on this connection are declared as stored entities",
		"fix", "set `maxmemory-policy` to `noeviction`, or to a `volatile-*` policy (which evicts only keys that were given a TTL) — or keep it, if these models really are a cache")
}

// EvictsUntaggedKeys reports whether `policy` can evict a key stored without a
// TTL. Exported so the rule is stated ONCE: `volatile-*` touches only keys
// someone gave an expiry, and `noeviction` fails the write instead — loudly,
// which is the posture a stored entity wants.
func EvictsUntaggedKeys(policy string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(policy)), "allkeys-")
}
