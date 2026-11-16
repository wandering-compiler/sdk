//! The transaction client against a scripted coordinator, for the cases Go's
//! client (sdk/go/service/tx/distx, D3.3 / D3.9 / D3.15-D3.17) settles: a
//! lost commit reply read from the coordinator's ended record, a commit
//! refused because a call still holds the transaction (the parked work waits
//! for the retry; `run` rolls back), a rollback of a transaction that already
//! ended, and the bound on every detached rollback.

use std::{
    collections::{HashSet, VecDeque},
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use prost::Message as _;
use tonic::{Code, Request, Response, Status};

use crate::{
    Ctx,
    channel::{Outbound, StorageChannel},
    distx::{self, Coordinator},
    errors::encode_rpc_status,
    on_commit,
    pb::{
        google::protobuf::Any,
        w17::common::distx::{
            BeginRequest, BeginResponse, CommitRequest, CommitResponse, RollbackRequest,
            RollbackResponse,
            w17_distributed_transaction_server::{
                W17DistributedTransaction, W17DistributedTransactionServer,
            },
        },
    },
};

/// What the coordinator answers, call by call; an empty queue answers OK.
#[derive(Default)]
struct Script {
    calls: Mutex<Vec<&'static str>>,
    commits: Mutex<VecDeque<Status>>,
    rollbacks: Mutex<VecDeque<Status>>,
    /// Accept every rollback and never answer.
    stall_rollback: bool,
}

impl Script {
    fn seq(&self) -> String {
        self.calls.lock().unwrap().join(",")
    }
}

struct Fake(Arc<Script>);

#[tonic::async_trait]
impl W17DistributedTransaction for Fake {
    async fn begin(&self, _: Request<BeginRequest>) -> Result<Response<BeginResponse>, Status> {
        self.0.calls.lock().unwrap().push("begin");
        Ok(Response::new(BeginResponse {
            conn_id: String::new(),
            tx_id: "tx-1".into(),
        }))
    }

    async fn commit(&self, _: Request<CommitRequest>) -> Result<Response<CommitResponse>, Status> {
        self.0.calls.lock().unwrap().push("commit");
        match self.0.commits.lock().unwrap().pop_front() {
            Some(st) => Err(st),
            None => Ok(Response::new(CommitResponse {})),
        }
    }

    async fn rollback(
        &self,
        _: Request<RollbackRequest>,
    ) -> Result<Response<RollbackResponse>, Status> {
        self.0.calls.lock().unwrap().push("rollback");
        if self.0.stall_rollback {
            std::future::pending::<()>().await;
        }
        match self.0.rollbacks.lock().unwrap().pop_front() {
            Some(st) => Err(st),
            None => Ok(Response::new(RollbackResponse {})),
        }
    }
}

async fn coordinator(script: Arc<Script>) -> Coordinator {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(W17DistributedTransactionServer::new(Fake(script)))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener)),
    );
    let ch = tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
        .unwrap()
        .connect_lazy();
    Coordinator::new(StorageChannel(Arc::new(Outbound::new(
        ch,
        false,
        HashSet::new(),
    ))))
}

/// The NotFound the coordinator answers for a transaction it remembers.
fn ended(kind: &str) -> Status {
    let info = crate::pb::google::rpc::ErrorInfo {
        reason: "W17_TX_ENDED".into(),
        domain: "w17.distx".into(),
        metadata: [("ended".to_string(), kind.to_string())]
            .into_iter()
            .collect(),
    };
    let any = Any {
        type_url: "type.googleapis.com/google.rpc.ErrorInfo".into(),
        value: info.encode_to_vec(),
    };
    let msg = "distx: unknown transaction";
    Status::with_details(
        Code::NotFound,
        msg,
        encode_rpc_status(Code::NotFound, msg, vec![any]),
    )
}

fn script(commits: Vec<Status>, rollbacks: Vec<Status>) -> Arc<Script> {
    Arc::new(Script {
        commits: Mutex::new(commits.into()),
        rollbacks: Mutex::new(rollbacks.into()),
        ..Default::default()
    })
}

async fn park(ctx: &Ctx, ran: &Arc<AtomicUsize>) {
    let ran = ran.clone();
    on_commit(ctx, async move {
        ran.fetch_add(1, Ordering::SeqCst);
    })
    .await
    .unwrap();
}

fn rpc_code(e: &distx::Error) -> Option<Code> {
    match e {
        distx::Error::Rpc(s) => Some(s.code()),
        _ => None,
    }
}

#[tokio::test]
async fn a_lost_commit_reply_settled_as_committed_runs_the_parked_work() {
    let s = script(
        vec![Status::deadline_exceeded("reply lost")],
        vec![ended("committed")],
    );
    let coord = coordinator(s.clone()).await;
    let ran = Arc::new(AtomicUsize::new(0));
    let (h, ctx) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
        .await
        .unwrap();
    park(&ctx, &ran).await;
    h.commit(&Ctx::background())
        .await
        .expect("the coordinator recorded the commit as landed");
    assert_eq!(ran.load(Ordering::SeqCst), 1);
    assert_eq!(s.seq(), "begin,commit,rollback");
    let again = h.commit(&Ctx::background()).await.unwrap_err();
    assert!(again.to_string().contains("already committed"), "{again}");
}

#[tokio::test]
async fn a_lost_commit_reply_settled_as_not_committed_is_an_ordinary_failure() {
    let s = script(
        vec![Status::deadline_exceeded("lost")],
        vec![ended("not_committed")],
    );
    let coord = coordinator(s.clone()).await;
    let ran = Arc::new(AtomicUsize::new(0));
    let (h, ctx) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
        .await
        .unwrap();
    park(&ctx, &ran).await;
    let e = h.commit(&Ctx::background()).await.unwrap_err();
    assert_eq!(rpc_code(&e), Some(Code::DeadlineExceeded), "{e}");
    assert_eq!(ran.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn a_busy_commit_keeps_the_parked_work_for_the_retry() {
    let s = script(
        vec![Status::failed_precondition("a call still holds it")],
        vec![],
    );
    let coord = coordinator(s.clone()).await;
    let ran = Arc::new(AtomicUsize::new(0));
    let (h, ctx) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
        .await
        .unwrap();
    park(&ctx, &ran).await;
    let e = h.commit(&Ctx::background()).await.unwrap_err();
    assert_eq!(rpc_code(&e), Some(Code::FailedPrecondition));
    h.commit(&Ctx::background()).await.unwrap();
    assert_eq!(s.seq(), "begin,commit,commit");
    assert_eq!(
        ran.load(Ordering::SeqCst),
        1,
        "the retry's commit must run the work parked before the busy refusal"
    );
}

#[tokio::test]
async fn run_rolls_back_a_busy_commit() {
    let s = script(
        vec![Status::failed_precondition("a call still holds it")],
        vec![],
    );
    let coord = coordinator(s.clone()).await;
    let r: Result<(), distx::Error> = distx::run(
        &Ctx::background(),
        &coord,
        distx::begin_request("main"),
        async |_| Ok(()),
    )
    .await;
    assert_eq!(rpc_code(&r.unwrap_err()), Some(Code::FailedPrecondition));
    assert_eq!(s.seq(), "begin,commit,rollback");
}

#[tokio::test]
async fn a_rollback_of_a_transaction_that_ended_without_committing_is_done() {
    let s = script(vec![], vec![ended("not_committed")]);
    let coord = coordinator(s.clone()).await;
    let (h, _) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
        .await
        .unwrap();
    h.rollback(&Ctx::background())
        .await
        .expect("already ended without committing is what a rollback asks for");
    let again = h.rollback(&Ctx::background()).await.unwrap_err();
    assert!(again.to_string().contains("already rolled back"), "{again}");
}

#[tokio::test]
async fn a_rollback_of_a_committed_transaction_names_it() {
    let s = script(vec![], vec![ended("committed")]);
    let coord = coordinator(s.clone()).await;
    let (h, _) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
        .await
        .unwrap();
    let e = h.rollback(&Ctx::background()).await.unwrap_err();
    assert!(e.to_string().contains("already committed"), "{e}");
}

#[test]
fn the_rollback_bound_is_the_finish_wait() {
    assert_eq!(distx::ROLLBACK_WAIT, Duration::from_secs(30));
}

async fn bounded(path: &str) {
    let s = Arc::new(Script {
        stall_rollback: true,
        ..Default::default()
    });
    let coord = coordinator(s.clone())
        .await
        .with_rollback_wait(Duration::from_millis(50));
    let go = async {
        match path {
            "rollback" => {
                let (h, _) = distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
                    .await
                    .unwrap();
                let _ = h.rollback(&Ctx::background()).await;
            }
            "run" => {
                let _: Result<(), distx::Error> = distx::run(
                    &Ctx::background(),
                    &coord,
                    distx::begin_request("main"),
                    async |_| Err(distx::Error::Rpc(Status::internal("boom"))),
                )
                .await;
            }
            _ => {
                let (h, ctx) =
                    distx::begin(&Ctx::background(), &coord, distx::begin_request("main"))
                        .await
                        .unwrap();
                let (inner, _) = distx::begin(&ctx, &coord, distx::begin_request("main"))
                    .await
                    .unwrap();
                inner.rollback(&ctx).await.unwrap();
                let _ = h.commit(&Ctx::background()).await;
            }
        }
    };
    tokio::time::timeout(Duration::from_secs(5), go)
        .await
        .unwrap_or_else(|_| {
            panic!("{path}: a detached rollback against a stalled coordinator was not bounded")
        });
}

#[tokio::test]
async fn a_detached_rollback_is_bounded_rollback() {
    bounded("rollback").await;
}

#[tokio::test]
async fn a_detached_rollback_is_bounded_run() {
    bounded("run").await;
}

#[tokio::test]
async fn a_detached_rollback_is_bounded_rollback_only() {
    bounded("rollback_only").await;
}
