//! Body-blind routing decision + HTTP/2 forwarding.
//!
//! The router inspects only the HTTP/2 `:path` pseudo-header and the
//! `w17-conn-id` request header. It **never** decodes a protobuf message
//! body — request and response bodies are forwarded as opaque streams.
//!
//! The pure decision is split out into [`decide`] so the whole routing
//! table is unit-testable from `(path, conn_id, map-state)` with no I/O.

use std::sync::Arc;

use crate::affinity::AffinityMap;
use crate::lb::RoundRobin;

/// gRPC method names on the distributed-tx service, matched as a `:path`
/// suffix so the package prefix is irrelevant (contract-blind).
/// Reserved for metrics/log labelling (slice S5); routing uses the
/// exact suffixes below.
#[allow(dead_code)]
const SVC: &str = "/w17.common.distx.W17DistributedTransaction/";
const BEGIN_SUFFIX: &str = "/w17.common.distx.W17DistributedTransaction/Begin";
const COMMIT_SUFFIX: &str = "/w17.common.distx.W17DistributedTransaction/Commit";
const ROLLBACK_SUFFIX: &str = "/w17.common.distx.W17DistributedTransaction/Rollback";

/// Request header carrying the proxy-minted affinity token.
pub const CONN_ID_HEADER: &str = "w17-conn-id";

/// The four request classes the proxy distinguishes by metadata alone.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Class {
    /// `Begin` — mint a conn_id, pin a fresh replica.
    Begin,
    /// In-transaction RPC carrying a known `w17-conn-id`.
    InTx,
    /// `Commit`/`Rollback` carrying a `w17-conn-id` — route then evict.
    Finish,
    /// No `w17-conn-id` — free round-robin.
    LoadBalanced,
    /// In-tx/finish RPC whose `w17-conn-id` is unknown or expired.
    UnknownConn,
}

/// The concrete routing outcome for one request.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Route {
    /// Forward to `replica`. If `stamp_conn_id` is set, inject that
    /// `w17-conn-id` into the **response** metadata (the Begin case).
    /// If `evict` is set, drop that affinity entry after forwarding
    /// (the Commit/Rollback case).
    Forward {
        replica: usize,
        class: Class,
        stamp_conn_id: Option<String>,
        evict: Option<String>,
    },
    /// Do not forward: reply with a gRPC `FAILED_PRECONDITION` (status 9).
    /// Used when an in-tx/finish RPC names an unknown/expired conn_id.
    Reject {
        class: Class,
        grpc_status: i32,
        message: String,
    },
}

impl Route {
    /// The request class of this routing outcome (for metrics/span labels).
    pub fn class(&self) -> Class {
        match self {
            Route::Forward { class, .. } => *class,
            Route::Reject { class, .. } => *class,
        }
    }
}

/// gRPC status code FAILED_PRECONDITION.
pub const GRPC_FAILED_PRECONDITION: i32 = 9;

/// Minimal injectable "minter" so the Begin path can be tested with a
/// deterministic conn_id. Production uses [`UuidMinter`].
pub trait ConnIdMinter: Send + Sync {
    fn mint(&self) -> String;
}

/// Mints UUID v4 conn_ids.
pub struct UuidMinter;
impl ConnIdMinter for UuidMinter {
    fn mint(&self) -> String {
        uuid::Uuid::new_v4().to_string()
    }
}

/// Classify a request purely from its `:path` and optional `w17-conn-id`.
///
/// Note: classification is independent of map state — a `Commit` with a
/// conn_id is `Finish` even if the id is later found expired (the
/// decision step then turns that into a reject).
pub fn classify(path: &str, conn_id: Option<&str>) -> Class {
    let is_begin = path.ends_with(BEGIN_SUFFIX);
    let is_finish = path.ends_with(COMMIT_SUFFIX) || path.ends_with(ROLLBACK_SUFFIX);
    match (is_begin, is_finish, conn_id) {
        (true, _, _) => Class::Begin,
        (false, true, Some(_)) => Class::Finish,
        // Commit/Rollback without a conn_id falls through to free LB
        // (single-replica caller that never got stamped) — handled below.
        (false, _, Some(_)) => Class::InTx,
        (false, _, None) => Class::LoadBalanced,
    }
}

/// Pure routing decision. Given the request's path + conn_id and the
/// shared affinity map + balancer, produce a [`Route`] without any I/O.
///
/// This is the function the whole routing table is tested against.
pub fn decide(
    path: &str,
    conn_id: Option<&str>,
    affinity: &AffinityMap,
    lb: &RoundRobin,
    minter: &dyn ConnIdMinter,
) -> Route {
    match classify(path, conn_id) {
        Class::Begin => {
            let replica = lb.pick();
            let id = minter.mint();
            affinity.insert(id.clone(), replica);
            Route::Forward {
                replica,
                class: Class::Begin,
                stamp_conn_id: Some(id),
                evict: None,
            }
        }
        Class::Finish => {
            let id = conn_id.expect("Finish implies conn_id present");
            match affinity.lookup(id) {
                Some(replica) => Route::Forward {
                    replica,
                    class: Class::Finish,
                    stamp_conn_id: None,
                    evict: Some(id.to_string()),
                },
                None => reject_unknown(id),
            }
        }
        Class::InTx => {
            let id = conn_id.expect("InTx implies conn_id present");
            match affinity.lookup(id) {
                Some(replica) => Route::Forward {
                    replica,
                    class: Class::InTx,
                    stamp_conn_id: None,
                    evict: None,
                },
                None => reject_unknown(id),
            }
        }
        Class::LoadBalanced => Route::Forward {
            replica: lb.pick(),
            class: Class::LoadBalanced,
            stamp_conn_id: None,
            evict: None,
        },
        // classify never returns these, but keep decide total.
        Class::UnknownConn => reject_unknown(conn_id.unwrap_or("")),
    }
}

fn reject_unknown(id: &str) -> Route {
    Route::Reject {
        class: Class::UnknownConn,
        grpc_status: GRPC_FAILED_PRECONDITION,
        message: format!(
            "w17-conn-id {id:?} is unknown or expired: the pinned storage \
             replica churned or the transaction timed out; retry the whole \
             transaction"
        ),
    }
}

/// Shared routing state handed to every connection.
pub struct Router {
    pub affinity: Arc<AffinityMap>,
    pub lb: Arc<RoundRobin>,
    pub minter: Arc<dyn ConnIdMinter>,
    /// Replica authorities indexed by the integers [`decide`] returns.
    pub replicas: Vec<String>,
}

impl Router {
    /// Resolve a replica index to its `host:port` authority.
    pub fn authority(&self, replica: usize) -> &str {
        &self.replicas[replica]
    }

    /// True if `path` targets the distributed-tx service at all (used
    /// only for logging/metrics labelling; routing uses [`decide`]).
    #[allow(dead_code)]
    pub fn is_distx_path(path: &str) -> bool {
        path.contains(SVC)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::Duration;

    struct FixedMinter(String);
    impl ConnIdMinter for FixedMinter {
        fn mint(&self) -> String {
            self.0.clone()
        }
    }

    /// Minter handing out a queue of ids in order.
    struct SeqMinter(Mutex<Vec<String>>);
    impl ConnIdMinter for SeqMinter {
        fn mint(&self) -> String {
            self.0.lock().unwrap().remove(0)
        }
    }

    fn map() -> AffinityMap {
        AffinityMap::new(Duration::from_secs(300))
    }

    const PREFIX: &str = "/some.project.storage";
    fn begin_path() -> String {
        format!("{PREFIX}{BEGIN_SUFFIX}")
    }
    fn commit_path() -> String {
        format!("{PREFIX}{COMMIT_SUFFIX}")
    }
    fn rollback_path() -> String {
        format!("{PREFIX}{ROLLBACK_SUFFIX}")
    }

    // ---- classify ---------------------------------------------------

    #[test]
    fn classify_begin() {
        assert_eq!(classify(&begin_path(), None), Class::Begin);
        // Begin wins even if a conn_id is (spuriously) present.
        assert_eq!(classify(&begin_path(), Some("x")), Class::Begin);
    }

    #[test]
    fn classify_commit_and_rollback_with_conn_id_is_finish() {
        assert_eq!(classify(&commit_path(), Some("c")), Class::Finish);
        assert_eq!(classify(&rollback_path(), Some("c")), Class::Finish);
    }

    #[test]
    fn classify_commit_without_conn_id_is_load_balanced() {
        assert_eq!(classify(&commit_path(), None), Class::LoadBalanced);
    }

    #[test]
    fn classify_in_tx_rpc() {
        let p = "/app.notes.NotesMutation/Update";
        assert_eq!(classify(p, Some("c")), Class::InTx);
        assert_eq!(classify(p, None), Class::LoadBalanced);
    }

    // ---- decide -----------------------------------------------------

    #[test]
    fn begin_mints_pins_and_stamps() {
        let m = map();
        let lb = RoundRobin::new(3);
        let minter = FixedMinter("conn-abc".into());
        let route = decide(&begin_path(), None, &m, &lb, &minter);
        match route {
            Route::Forward {
                replica,
                class,
                stamp_conn_id,
                evict,
            } => {
                assert_eq!(class, Class::Begin);
                assert_eq!(replica, 0);
                assert_eq!(stamp_conn_id.as_deref(), Some("conn-abc"));
                assert!(evict.is_none());
            }
            other => panic!("expected Forward, got {other:?}"),
        }
        // Affinity now pins conn-abc -> 0.
        assert_eq!(m.lookup("conn-abc"), Some(0));
    }

    #[test]
    fn followup_in_tx_hits_pinned_replica() {
        let m = map();
        let lb = RoundRobin::new(3);
        // Pin via Begin first (replica 0), then advance LB so a naive
        // round-robin would diverge — affinity must win.
        let minter = FixedMinter("c1".into());
        decide(&begin_path(), None, &m, &lb, &minter);
        let _ = lb.pick(); // LB now at 1
        let route = decide(
            "/app.notes.NotesMutation/Update",
            Some("c1"),
            &m,
            &lb,
            &minter,
        );
        match route {
            Route::Forward { replica, class, .. } => {
                assert_eq!(replica, 0, "must hit the Begin replica, not the LB cursor");
                assert_eq!(class, Class::InTx);
            }
            other => panic!("expected Forward, got {other:?}"),
        }
    }

    #[test]
    fn commit_routes_then_evicts() {
        let m = map();
        let lb = RoundRobin::new(2);
        let minter = FixedMinter("c1".into());
        decide(&begin_path(), None, &m, &lb, &minter);
        let route = decide(&commit_path(), Some("c1"), &m, &lb, &minter);
        match route {
            Route::Forward {
                replica,
                class,
                evict,
                ..
            } => {
                assert_eq!(class, Class::Finish);
                assert_eq!(replica, 0);
                assert_eq!(evict.as_deref(), Some("c1"));
            }
            other => panic!("expected Forward, got {other:?}"),
        }
        // decide() does not itself evict — the forwarder evicts after the
        // upstream call. The entry is still present here.
        assert_eq!(m.lookup("c1"), Some(0));
    }

    #[test]
    fn unknown_conn_id_in_tx_rejected() {
        let m = map();
        let lb = RoundRobin::new(2);
        let minter = FixedMinter("c1".into());
        let route = decide("/app.notes.NotesQuery/Get", Some("ghost"), &m, &lb, &minter);
        match route {
            Route::Reject {
                class, grpc_status, ..
            } => {
                assert_eq!(class, Class::UnknownConn);
                assert_eq!(grpc_status, GRPC_FAILED_PRECONDITION);
            }
            other => panic!("expected Reject, got {other:?}"),
        }
    }

    #[test]
    fn unknown_conn_id_commit_rejected() {
        let m = map();
        let lb = RoundRobin::new(2);
        let minter = FixedMinter("c1".into());
        let route = decide(&commit_path(), Some("ghost"), &m, &lb, &minter);
        assert!(matches!(route, Route::Reject { .. }));
    }

    #[test]
    fn non_tx_rpc_round_robins() {
        let m = map();
        let lb = RoundRobin::new(3);
        let minter = FixedMinter("unused".into());
        let p = "/app.notes.NotesQuery/List";
        let replicas: Vec<usize> = (0..4)
            .map(|_| match decide(p, None, &m, &lb, &minter) {
                Route::Forward { replica, class, .. } => {
                    assert_eq!(class, Class::LoadBalanced);
                    replica
                }
                other => panic!("expected Forward, got {other:?}"),
            })
            .collect();
        assert_eq!(replicas, vec![0, 1, 2, 0]);
    }

    #[test]
    fn two_begins_pin_distinct_replicas() {
        let m = map();
        let lb = RoundRobin::new(3);
        let minter = SeqMinter(Mutex::new(vec!["a".into(), "b".into()]));
        let r1 = decide(&begin_path(), None, &m, &lb, &minter);
        let r2 = decide(&begin_path(), None, &m, &lb, &minter);
        let rep = |r: Route| match r {
            Route::Forward { replica, .. } => replica,
            other => panic!("{other:?}"),
        };
        assert_eq!(rep(r1), 0);
        assert_eq!(rep(r2), 1);
        assert_eq!(m.lookup("a"), Some(0));
        assert_eq!(m.lookup("b"), Some(1));
    }

    #[test]
    fn is_distx_path_matches_service() {
        assert!(Router::is_distx_path(&begin_path()));
        assert!(!Router::is_distx_path("/app.notes.NotesQuery/Get"));
    }
}
