package kvposture

import "testing"

// The rule the whole package turns on, stated once and pinned here: only an
// `allkeys-*` policy can evict a key that was stored without a TTL.
// `volatile-*` touches only keys someone gave an expiry — which for a w17 KV
// model means the ones that DECLARED a `ttl_seconds` — and `noeviction` fails
// the write loudly instead, which is what a stored entity wants.
func TestEvictsUntaggedKeys(t *testing.T) {
	cases := map[string]bool{
		"allkeys-lru":     true,
		"allkeys-lfu":     true,
		"allkeys-random":  true,
		"ALLKEYS-LRU":     true,
		" allkeys-lru \n": true,
		"noeviction":      false,
		"volatile-lru":    false,
		"volatile-ttl":    false,
		"volatile-random": false,
		"":                false,
	}
	for policy, want := range cases {
		if got := EvictsUntaggedKeys(policy); got != want {
			t.Errorf("EvictsUntaggedKeys(%q) = %v, want %v", policy, got, want)
		}
	}
}

// A nil client is the boot path where the connection never came up; the check
// must not be the thing that panics there.
func TestWarnIfEvicting_NilClientIsSafe(t *testing.T) {
	WarnIfEvicting(t.Context(), "main", nil)
}
