//! `conn_id -> replica_index` affinity map with per-entry TTL.
//!
//! The proxy mints an opaque `conn_id` on `Begin` and pins every
//! subsequent RPC of that logical transaction to the same replica.
//! Entries expire after a TTL aligned with the Go storage
//! `distx.DefaultOrphanTimeout` (5 min) so a dropped caller cannot leak
//! map entries; the backend's own orphan-rollback watcher reclaims the
//! `*sql.Tx` independently.
//!
//! The TTL is an IDLE timeout: every routed call of the transaction
//! ([`AffinityMap::lookup`]) starts it again. Counted from `Begin` alone, a
//! transaction that is busy past it — one whose caller chose a
//! `tx_timeout_ms` longer than the TTL — lost its route mid-way, and its
//! Rollback with it, while storage still held it open. Storage stays the
//! authority on how long a transaction may live: past its own timeout it
//! answers the routed call itself.
//!
//! Clock is injected (`Clock` trait) so TTL expiry is deterministically
//! unit-testable without sleeping.

use std::collections::HashMap;
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

/// Monotonic clock abstraction (injectable for tests).
pub trait Clock: Send + Sync {
    fn now(&self) -> Instant;
}

/// Real monotonic clock.
pub struct SystemClock;
impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }
}

struct Entry {
    replica: usize,
    expires_at: Instant,
}

/// Thread-safe affinity map. Lookups lazily drop expired entries.
pub struct AffinityMap {
    ttl: Duration,
    max_entries: usize,
    clock: Box<dyn Clock>,
    inner: Mutex<HashMap<String, Entry>>,
}

/// How many transactions one proxy pins at once. A Begin past it is refused
/// (RESOURCE_EXHAUSTED) rather than growing the map without bound; live
/// entries are open transactions, so this is far above any real load.
pub const MAX_ENTRIES: usize = 100_000;

impl AffinityMap {
    pub fn new(ttl: Duration) -> AffinityMap {
        AffinityMap::with_clock(ttl, Box::new(SystemClock))
    }

    pub fn with_clock(ttl: Duration, clock: Box<dyn Clock>) -> AffinityMap {
        AffinityMap {
            ttl,
            max_entries: MAX_ENTRIES,
            clock,
            inner: Mutex::new(HashMap::new()),
        }
    }

    /// The same map holding at most `n` entries.
    pub fn with_max_entries(mut self, n: usize) -> AffinityMap {
        self.max_entries = n;
        self
    }

    /// Acquire the map guard, tolerating lock poisoning. This is the
    /// single shared mutable state every routed request touches; a
    /// bare `lock().unwrap()` would, if any thread ever panicked while
    /// holding the lock, poison it and turn every subsequent request
    /// (plus the reaper and the metrics callback) into a panic —
    /// a permanent, total proxy outage. The code under this lock only
    /// performs HashMap ops, so the map is always left in a consistent
    /// state; recovering the poisoned guard via `into_inner()` is
    /// strictly safer than crashing the proxy.
    fn guard(&self) -> MutexGuard<'_, HashMap<String, Entry>> {
        self.inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Pin `conn_id` to `replica`, (re)setting its TTL. False — and nothing
    /// pinned — when the map is full of live entries (expired ones are
    /// dropped first).
    pub fn insert(&self, conn_id: String, replica: usize) -> bool {
        let now = self.clock.now();
        let mut map = self.guard();
        if map.len() >= self.max_entries && !map.contains_key(&conn_id) {
            map.retain(|_, e| e.expires_at > now);
            if map.len() >= self.max_entries {
                return false;
            }
        }
        map.insert(
            conn_id,
            Entry {
                replica,
                expires_at: now + self.ttl,
            },
        );
        true
    }

    /// Look up the replica for `conn_id` and start its TTL again (the
    /// transaction is in use). Returns `None` if absent or expired (an
    /// expired entry is dropped as a side effect).
    pub fn lookup(&self, conn_id: &str) -> Option<usize> {
        let now = self.clock.now();
        let mut map = self.guard();
        match map.get_mut(conn_id) {
            Some(e) if e.expires_at > now => {
                e.expires_at = now + self.ttl;
                Some(e.replica)
            }
            Some(_) => {
                map.remove(conn_id);
                None
            }
            None => None,
        }
    }

    /// Remove `conn_id` (Commit/Rollback). Returns the replica it was
    /// pinned to, if it was still live.
    pub fn evict(&self, conn_id: &str) -> Option<usize> {
        let now = self.clock.now();
        let mut map = self.guard();
        map.remove(conn_id)
            .filter(|e| e.expires_at > now)
            .map(|e| e.replica)
    }

    /// Current number of (possibly-expired-but-not-yet-reaped) entries.
    /// Exposed for the `affinity_map_size` metric (slice S5).
    pub fn len(&self) -> usize {
        self.guard().len()
    }

    /// True when the map holds no entries.
    pub fn is_empty(&self) -> bool {
        self.guard().is_empty()
    }

    /// Drop all expired entries. Cheap periodic GC for the metric to
    /// stay honest even without lookups; callable from a background task.
    pub fn reap_expired(&self) -> usize {
        let now = self.clock.now();
        let mut map = self.guard();
        let before = map.len();
        map.retain(|_, e| e.expires_at > now);
        before - map.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;

    /// Test clock advanced manually.
    struct FakeClock {
        base: Instant,
        offset_ms: Arc<AtomicU64>,
    }
    impl FakeClock {
        fn new() -> (FakeClock, Arc<AtomicU64>) {
            let offset = Arc::new(AtomicU64::new(0));
            (
                FakeClock {
                    base: Instant::now(),
                    offset_ms: offset.clone(),
                },
                offset,
            )
        }
    }
    impl Clock for FakeClock {
        fn now(&self) -> Instant {
            self.base + Duration::from_millis(self.offset_ms.load(Ordering::SeqCst))
        }
    }

    fn map_with_clock(ttl_ms: u64) -> (AffinityMap, Arc<AtomicU64>) {
        let (clock, offset) = FakeClock::new();
        (
            AffinityMap::with_clock(Duration::from_millis(ttl_ms), Box::new(clock)),
            offset,
        )
    }

    #[test]
    fn insert_then_lookup() {
        let (m, _) = map_with_clock(1000);
        m.insert("c1".into(), 2);
        assert_eq!(m.lookup("c1"), Some(2));
    }

    #[test]
    fn unknown_id_is_none() {
        let (m, _) = map_with_clock(1000);
        assert_eq!(m.lookup("nope"), None);
    }

    #[test]
    fn evict_returns_replica_and_removes() {
        let (m, _) = map_with_clock(1000);
        m.insert("c1".into(), 1);
        assert_eq!(m.evict("c1"), Some(1));
        assert_eq!(m.lookup("c1"), None);
        assert_eq!(m.evict("c1"), None);
    }

    #[test]
    fn lookup_after_ttl_expires() {
        let (m, clock) = map_with_clock(1000);
        m.insert("c1".into(), 3);
        clock.store(1001, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), None);
        // expired entry was reaped on lookup
        assert_eq!(m.len(), 0);
    }

    #[test]
    fn lookup_just_before_ttl_still_live() {
        let (m, clock) = map_with_clock(1000);
        m.insert("c1".into(), 3);
        clock.store(999, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), Some(3));
    }

    #[test]
    fn a_full_map_refuses_until_an_entry_expires() {
        let (m, clock) = map_with_clock(1000);
        let m = m.with_max_entries(2);
        assert!(m.insert("c1".into(), 0));
        assert!(m.insert("c2".into(), 0));
        assert!(!m.insert("c3".into(), 0), "full of live entries");
        assert!(
            m.insert("c2".into(), 1),
            "re-pinning a live entry is not growth"
        );
        assert_eq!(m.lookup("c3"), None);
        clock.fetch_add(1001, Ordering::SeqCst);
        assert!(m.insert("c3".into(), 0), "expired entries make room");
        assert_eq!(m.len(), 1);
    }

    #[test]
    fn evict_expired_returns_none() {
        let (m, clock) = map_with_clock(1000);
        m.insert("c1".into(), 3);
        clock.store(2000, Ordering::SeqCst);
        assert_eq!(m.evict("c1"), None);
    }

    #[test]
    fn insert_refreshes_ttl() {
        let (m, clock) = map_with_clock(1000);
        m.insert("c1".into(), 3);
        clock.store(900, Ordering::SeqCst);
        m.insert("c1".into(), 3); // refresh: now expires at 1900
        clock.store(1800, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), Some(3));
    }

    #[test]
    fn lookup_refreshes_ttl() {
        let (m, clock) = map_with_clock(1000);
        m.insert("c1".into(), 3);
        clock.store(900, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), Some(3)); // in use: now expires at 1900
        clock.store(1800, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), Some(3)); // and now at 2800
        clock.store(2900, Ordering::SeqCst);
        assert_eq!(m.lookup("c1"), None, "idle past the TTL");
    }

    #[test]
    fn survives_poisoned_lock() {
        use std::panic::{catch_unwind, AssertUnwindSafe};
        let (m, _) = map_with_clock(1000);
        m.insert("c1".into(), 7);

        // Poison the inner mutex: panic while holding its guard (the
        // guard is dropped during unwind, which marks the Mutex
        // poisoned). catch_unwind keeps the panic from aborting the
        // test thread.
        let _ = catch_unwind(AssertUnwindSafe(|| {
            let _g = m.inner.lock().unwrap();
            panic!("poison the lock");
        }));

        // A bare `lock().unwrap()` would now panic on every call. The
        // poison-tolerant guard() keeps the map fully usable — the
        // proxy stays up instead of melting down.
        assert_eq!(m.lookup("c1"), Some(7));
        m.insert("c2".into(), 9);
        assert_eq!(m.lookup("c2"), Some(9));
        assert_eq!(m.len(), 2);
        assert_eq!(m.evict("c1"), Some(7));
    }

    #[test]
    fn reap_expired_drops_only_dead() {
        let (m, clock) = map_with_clock(1000);
        m.insert("a".into(), 0);
        clock.store(500, Ordering::SeqCst);
        m.insert("b".into(), 1); // expires at 1500
        clock.store(1200, Ordering::SeqCst); // a dead, b live
        let dropped = m.reap_expired();
        assert_eq!(dropped, 1);
        assert_eq!(m.lookup("a"), None);
        assert_eq!(m.lookup("b"), Some(1));
    }
}
