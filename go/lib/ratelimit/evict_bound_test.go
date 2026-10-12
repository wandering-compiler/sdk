package ratelimit

import (
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// freshKey names the i-th distinct client address.
func freshKey(i int) string {
	return netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String()
}

// fullLimiter returns a limiter at its key ceiling with every key live.
func fullLimiter(maxKeys int) *Limiter {
	l := New(Config{PerMinute: 30, Burst: 10, MaxKeys: maxKeys})
	for i := 0; i < maxKeys; i++ {
		l.allowKey(freshKey(i))
	}
	return l
}

// A stream of addresses the limiter has never seen — a botnet, or anything
// that rotates its source — must not cost a scan of every key per request
// under the lock every request takes. With the ceiling reached and all keys
// live, a few thousand fresh keys finish in milliseconds when eviction is
// constant-time; a scan of the whole map per new key takes seconds.
func TestLimiter_FreshKeysAtTheCeilingStayCheap(t *testing.T) {
	const maxKeys = DefaultMaxKeys
	l := fullLimiter(maxKeys)
	start := time.Now()
	for i := maxKeys; i < maxKeys+3000; i++ {
		l.allowKey(freshKey(i))
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("3000 fresh keys at the ceiling took %v; eviction must not scan every key per request", elapsed)
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > maxKeys {
		t.Fatalf("map grew past MaxKeys: %d buckets", n)
	}
}

// Eviction takes the least recently seen key: a client that keeps coming
// back keeps its bucket (and its spent tokens) while newcomers push out the
// ones that went quiet.
func TestLimiter_EvictsTheLeastRecentlySeen(t *testing.T) {
	l := New(Config{PerMinute: 1, Burst: 1, MaxKeys: 3, TTL: time.Hour})
	base := time.Now()
	tick := 0
	at := func() {
		tick++
		now := base.Add(time.Duration(tick) * time.Second)
		l.now = func() time.Time { return now }
	}

	at()
	if !l.allowKey("keep") {
		t.Fatal("first request must be admitted")
	}
	for i := 0; i < 10; i++ {
		at()
		l.allowKey("new-" + strconv.Itoa(i))
		at()
		// The returning client is refused — its one token is spent — which
		// is only true while its bucket survives.
		if l.allowKey("keep") {
			t.Fatalf("round %d: the returning client's bucket was evicted and refilled", i)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 3 {
		t.Fatalf("map grew past MaxKeys: %d buckets", len(l.buckets))
	}
	if _, ok := l.buckets["new-0"]; ok {
		t.Fatal("the least recently seen key survived eviction")
	}
}

// Idle keys go before live ones, even when they were created later.
func TestLimiter_EvictsIdleKeysFirst(t *testing.T) {
	l := New(Config{PerMinute: 60, Burst: 5, MaxKeys: 2, TTL: time.Minute})
	base := time.Now()
	set := func(d time.Duration) { now := base.Add(d); l.now = func() time.Time { return now } }

	set(0)
	l.allowKey("old-but-busy")
	set(time.Second)
	l.allowKey("idle")
	set(2 * time.Minute)
	l.allowKey("old-but-busy")
	set(2*time.Minute + time.Second)
	l.allowKey("newcomer")

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets["idle"]; ok {
		t.Fatal("an idle key survived while a live one was kept")
	}
	if _, ok := l.buckets["old-but-busy"]; !ok {
		t.Fatal("a live key was evicted before an idle one")
	}
}

func BenchmarkLimiter_FreshKeyAtTheCeiling(b *testing.B) {
	l := fullLimiter(DefaultMaxKeys)
	i := DefaultMaxKeys
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		l.allowKey(freshKey(i))
		i++
	}
}

func BenchmarkLimiter_KnownKey(b *testing.B) {
	l := fullLimiter(DefaultMaxKeys)
	key := freshKey(42)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		l.allowKey(key)
	}
}
