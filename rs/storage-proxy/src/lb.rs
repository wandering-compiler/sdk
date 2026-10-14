//! Round-robin load balancer over the configured replica set.
//!
//! Used for non-transactional RPCs (no `w17-conn-id`) and to pick the
//! replica that opens a new transaction (`Begin`). Picking a replica
//! returns its **index** so the affinity map stays endpoint-agnostic.

use std::sync::atomic::{AtomicUsize, Ordering};

/// A lock-free round-robin balancer over `n` replicas.
pub struct RoundRobin {
    len: usize,
    next: AtomicUsize,
}

impl RoundRobin {
    /// Build a balancer over `len` replicas. `len` must be > 0
    /// (config validation guarantees a non-empty replica set).
    pub fn new(len: usize) -> RoundRobin {
        assert!(len > 0, "RoundRobin needs at least one replica");
        RoundRobin {
            len,
            next: AtomicUsize::new(0),
        }
    }

    /// Return the index of the next replica, advancing the cursor.
    pub fn pick(&self) -> usize {
        let n = self.next.fetch_add(1, Ordering::Relaxed);
        n % self.len
    }

    /// Number of replicas. Reserved for the `affinity_map_size` /
    /// `replica_up` metrics (slice S5).
    #[allow(dead_code)]
    pub fn len(&self) -> usize {
        self.len
    }

    /// True when the ring has no backends.
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cycles_in_order() {
        let lb = RoundRobin::new(3);
        let seq: Vec<usize> = (0..7).map(|_| lb.pick()).collect();
        assert_eq!(seq, vec![0, 1, 2, 0, 1, 2, 0]);
    }

    #[test]
    fn single_replica_always_zero() {
        let lb = RoundRobin::new(1);
        assert_eq!(lb.pick(), 0);
        assert_eq!(lb.pick(), 0);
    }

    #[test]
    #[should_panic]
    fn zero_replicas_panics() {
        RoundRobin::new(0);
    }
}
