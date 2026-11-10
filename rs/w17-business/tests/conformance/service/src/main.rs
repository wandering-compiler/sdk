//! The conformance suite's NoteService, written against the Rust SDK.
//!
//! Kept as small and literal as the suite allows: each method exercises one
//! behaviour the Go harness asserts on the wire (see conformance.proto). It
//! is author code: a binary crate whose build.rs compiled the suite's
//! descriptor set.

mod pb {
    w17_business::include_protos!();
}

use std::time::{Duration, UNIX_EPOCH};

use bytes::Bytes;
use pb::clients::echo::{GetEchoRequest, echo_api_client::EchoApiClient};
use pb::conformance::v1::{
    self as v1,
    note_service_client::NoteServiceClient,
    note_service_server::{NoteService, NoteServiceServer},
    note_storage_client::NoteStorageClient,
};
use w17_business::{
    BootError, Clients, CompanionChannel, Config, Ctx, Metadata, Registry, Request, Response,
    SelfChannel, Status, StorageChannel, distx, files, on_commit, paging, principal,
};

// The storage connection the transactions open on (the harness registers "main").
const CONN: &str = "main";
const FIELD: &str = "conformance.v1.NoteFile.file";

w17_business::main!(register_business);

fn register_business(
    cfg: &Config,
    registry: &mut Registry,
    clients: &Clients,
) -> Result<(), BootError> {
    let notes = Notes {
        cfg: cfg.clone(),
        storage: NoteStorageClient::new(clients.storage()),
        tx: clients.distributed_transaction()?,
        // Composition goes through the served service, so the emit fires.
        me: NoteServiceClient::new(registry.self_channel()),
        echo: EchoApiClient::new(clients.egress("echo")?),
        files: clients.store("notes-files")?,
        media: clients.store("notes-media")?,
    };
    registry.register(NoteServiceServer::new(notes))
}

struct Notes {
    cfg: Config,
    storage: NoteStorageClient<StorageChannel>,
    tx: distx::Coordinator,
    me: NoteServiceClient<SelfChannel>,
    echo: EchoApiClient<CompanionChannel>,
    files: files::FileStore,
    media: files::FileStore,
}

fn insert(text: impl Into<String>) -> v1::InsertNoteRequest {
    v1::InsertNoteRequest { text: text.into() }
}

#[tonic::async_trait]
impl NoteService for Notes {
    async fn create_note(
        &self,
        req: Request<v1::CreateNoteRequest>,
    ) -> Result<Response<v1::Note>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let text = req.into_inner().text;
        let note = distx::run(
            &ctx,
            &self.tx,
            distx::begin_request(CONN),
            async |tx_ctx: Ctx| {
                Ok::<_, Status>(
                    self.storage
                        .clone()
                        .insert_note(tx_ctx.request(insert(text)))
                        .await?
                        .into_inner(),
                )
            },
        )
        .await?;
        Ok(Response::new(note))
    }

    async fn create_notes(
        &self,
        req: Request<v1::CreateNotesRequest>,
    ) -> Result<Response<v1::CreateNotesResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let r = req.into_inner();
        let mut notes = Vec::new();
        distx::run(
            &ctx,
            &self.tx,
            distx::begin_request(CONN),
            async |tx_ctx: Ctx| {
                for text in &r.texts {
                    if !r.nested_connection.is_empty() {
                        // A nested begin on another connection cannot join.
                        distx::run(
                            &tx_ctx,
                            &self.tx,
                            distx::begin_request(&r.nested_connection),
                            async |c: Ctx| {
                                self.storage
                                    .clone()
                                    .insert_note(c.request(insert(text.clone())))
                                    .await
                                    .map(|_| ())
                            },
                        )
                        .await?;
                    }
                    let req = v1::CreateNoteRequest { text: text.clone() };
                    notes.push(
                        self.me
                            .clone()
                            .create_note(tx_ctx.request(req))
                            .await?
                            .into_inner(),
                    );
                }
                if !r.nested_fail_text.is_empty() {
                    let req = v1::CreateNotesRequest {
                        texts: vec![r.nested_fail_text.clone()],
                        fail: true,
                        ..Default::default()
                    };
                    // swallowed on purpose: the root must still refuse to commit
                    let _ = self.me.clone().create_notes(tx_ctx.request(req)).await;
                }
                if !r.on_commit_text.is_empty() {
                    let detached = ctx.without_cancel();
                    let storage = self.storage.clone();
                    let text = r.on_commit_text.clone();
                    on_commit(&tx_ctx, async move {
                        storage
                            .clone()
                            .insert_note(detached.request(insert(text)))
                            .await
                    })
                    .await
                    .map_err(Status::internal)?;
                }
                if r.pause_ms > 0 {
                    tokio::time::sleep(Duration::from_millis(r.pause_ms as u64)).await;
                }
                if r.fail {
                    // An unexpected failure: answered INTERNAL, the transaction
                    // rolled back as the handler's future unwinds.
                    panic!("refusing after the writes");
                }
                Ok::<_, Status>(())
            },
        )
        .await?;
        Ok(Response::new(v1::CreateNotesResponse { notes }))
    }

    async fn list_notes(
        &self,
        req: Request<v1::ListNotesRequest>,
    ) -> Result<Response<v1::ListNotesResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        // Sibling calls that must NOT see the page, nor move it: one before
        // the forwarded call and one after it.
        self.storage
            .clone()
            .inspect(ctx.request(v1::InspectRequest::default()))
            .await?;
        let resp = self
            .storage
            .clone()
            .list_notes(paging::forward(&ctx).request(req.into_inner()))
            .await?;
        self.storage
            .clone()
            .inspect(ctx.request(v1::InspectRequest::default()))
            .await?;
        Ok(Response::new(resp.into_inner()))
    }

    async fn inspect(
        &self,
        req: Request<v1::InspectRequest>,
    ) -> Result<Response<v1::InspectResponse>, Status> {
        let mut ctx = w17_business::ctx(&req)?;
        let r = req.into_inner();
        if r.stale_outgoing {
            // A context reused from another request: the relay must replace
            // this whole set with the verified incoming principal.
            let stale = Metadata::from_pairs([
                ("x-w17-user", "stale-user"),
                ("x-w17-scope-org_id", "stale-org"),
                ("x-w17-audience", "stale-audience"),
                ("w17-label-tenant", "stale-tenant"),
                ("x-w17-scope-stale_only", "leak"),
            ]);
            ctx = ctx.with_outgoing(ctx.outgoing().merged(&stale));
        }
        Ok(Response::new(
            self.storage
                .clone()
                .inspect(ctx.request(r))
                .await?
                .into_inner(),
        ))
    }

    async fn refuse(&self, _req: Request<v1::RefuseRequest>) -> Result<Response<v1::Note>, Status> {
        Err(w17_business::for_user_dev(
            w17_business::Code::FailedPrecondition,
            "NOTE_LOCKED",
            "This note is locked.",
            "text",
            "note 7 is locked by an export",
        ))
    }

    async fn sleep(&self, req: Request<v1::SleepRequest>) -> Result<Response<v1::Note>, Status> {
        tokio::time::sleep(Duration::from_millis(req.into_inner().ms.max(0) as u64)).await;
        Ok(Response::new(v1::Note {
            text: "slept".into(),
            ..Default::default()
        }))
    }

    async fn tx_errors(
        &self,
        req: Request<v1::TxErrorsRequest>,
    ) -> Result<Response<v1::TxErrorsResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let case = req.into_inner().case;
        let result: Result<(), distx::Error> = async {
            match case.as_str() {
                "finished" => {
                    let (h, _) = distx::begin(&ctx, &self.tx, distx::begin_request(CONN)).await?;
                    h.commit(&ctx).await?;
                    h.commit(&ctx).await
                }
                "unknown" => {
                    let (h, tx_ctx) =
                        distx::begin(&ctx, &self.tx, distx::begin_request(CONN)).await?;
                    self.storage
                        .clone()
                        .insert_note(tx_ctx.request(insert("outcome-unknown")))
                        .await
                        .map_err(distx::Error::Rpc)?;
                    // Work parked on the transaction, which a commit of
                    // unknown outcome must not run: a composed method's
                    // event, and a hook.
                    let composed = v1::CreateNoteRequest {
                        text: "outcome-unknown-composed".into(),
                    };
                    self.me
                        .clone()
                        .create_note(tx_ctx.request(composed))
                        .await
                        .map_err(distx::Error::Rpc)?;
                    let detached = ctx.without_cancel();
                    let storage = self.storage.clone();
                    on_commit(&tx_ctx, async move {
                        storage
                            .clone()
                            .insert_note(detached.request(insert("after-outcome-unknown")))
                            .await
                    })
                    .await
                    .map_err(|e| distx::Error::Rpc(Status::internal(e)))?;
                    h.commit(&ctx).await
                }
                "mismatch" => {
                    let (h, tx_ctx) =
                        distx::begin(&ctx, &self.tx, distx::begin_request(CONN)).await?;
                    let r = distx::begin(&tx_ctx, &self.tx, distx::begin_request("other"))
                        .await
                        .map(|_| ());
                    let _ = h.rollback(&ctx).await;
                    r
                }
                "rollback_only" => {
                    distx::run(
                        &ctx,
                        &self.tx,
                        distx::begin_request(CONN),
                        async |tx_ctx: Ctx| {
                            let (h, _) =
                                distx::begin(&tx_ctx, &self.tx, distx::begin_request(CONN)).await?;
                            h.rollback(&tx_ctx).await // a joined rollback marks the root
                        },
                    )
                    .await
                }
                _ => Ok(()),
            }
        }
        .await;
        match result {
            Ok(()) => Ok(Response::new(v1::TxErrorsResponse::default())),
            Err(distx::Error::Rpc(st)) => Err(st),
            Err(e) => Ok(Response::new(v1::TxErrorsResponse {
                error: e.name().into(),
                message: e.to_string(),
            })),
        }
    }

    async fn whoami(
        &self,
        req: Request<v1::WhoamiRequest>,
    ) -> Result<Response<v1::WhoamiResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let token = self.cfg.secret("api_token")?;
        Ok(Response::new(v1::WhoamiResponse {
            user_id: principal::user_id(&ctx)?,
            org_id: principal::scope(&ctx, "org_id").unwrap_or_default(),
            label_tenant: principal::label(&ctx, "tenant").unwrap_or_default(),
            device_id: principal::device_id(&ctx).unwrap_or_default(),
            data_scope: principal::data_scope(&ctx),
            greeting: self.cfg.str("greeting")?,
            limit: self.cfg.int("limit")?,
            verbose: self.cfg.bool("verbose")?,
            token_repr: format!("{token}:{}", token.reveal().len()),
        }))
    }

    async fn echo(
        &self,
        req: Request<v1::EchoRequest>,
    ) -> Result<Response<v1::EchoResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let id = req.into_inner().id;
        // Inside a transaction: the companion call must not carry it.
        let reply = distx::run(
            &ctx,
            &self.tx,
            distx::begin_request(CONN),
            async |tx_ctx: Ctx| {
                Ok::<_, Status>(
                    self.echo
                        .clone()
                        .get_echo(tx_ctx.request(GetEchoRequest { id }))
                        .await?
                        .into_inner(),
                )
            },
        )
        .await?;
        Ok(Response::new(v1::EchoResponse {
            message: reply.message,
        }))
    }

    async fn attach_file(
        &self,
        req: Request<v1::AttachFileRequest>,
    ) -> Result<Response<v1::AttachFileResponse>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let r = req.into_inner();
        match r.case.as_str() {
            "read_only_delete" => {
                let refused = match self.media.delete(&ctx, "any").await {
                    Err(e) => e.to_string(),
                    Ok(()) => String::new(),
                };
                return Ok(Response::new(v1::AttachFileResponse {
                    refused,
                    ..Default::default()
                }));
            }
            "body_fails" => {
                // Paced, so the chunks before the failure reach the companion.
                let body = futures::stream::unfold(0, |i| async move {
                    match i {
                        0..25 => {
                            tokio::time::sleep(Duration::from_millis(10)).await;
                            Some((Ok(Bytes::from(vec![0u8; 8 << 10])), i + 1))
                        }
                        25 => Some((
                            Err(std::io::Error::other("conformance: the body failed")),
                            i + 1,
                        )),
                        _ => None,
                    }
                });
                let resp = match self.files.put(&ctx, FIELD, files::Body::stream(body)).await {
                    Ok(handle) => v1::AttachFileResponse {
                        handle,
                        ..Default::default()
                    },
                    Err(e) => v1::AttachFileResponse {
                        refused: e.to_string(),
                        ..Default::default()
                    },
                };
                return Ok(Response::new(resp));
            }
            "body_stalls" => {
                let body = futures::stream::unfold(0, |i| async move {
                    match i {
                        0 => Some((
                            Ok::<_, std::io::Error>(Bytes::from(vec![0u8; 192 << 10])),
                            1,
                        )),
                        1 => {
                            tokio::time::sleep(Duration::from_secs(10)).await;
                            Some((Ok(Bytes::from_static(&[0])), 2))
                        }
                        _ => None,
                    }
                });
                let handle = self
                    .files
                    .put(&ctx, FIELD, files::Body::stream(body))
                    .await?;
                return Ok(Response::new(v1::AttachFileResponse {
                    handle,
                    ..Default::default()
                }));
            }
            _ => {}
        }
        let size = r.size.max(0) as usize;
        let part = r.part.max(0) as usize;
        let body: Vec<u8> = (0..size).map(|i| (i % 251) as u8).collect();
        let resp = distx::run(
            &ctx,
            &self.tx,
            distx::begin_request(CONN),
            async |tx_ctx: Ctx| {
                let put_body = if part > 0 {
                    let parts: Vec<Result<Bytes, std::io::Error>> = body
                        .chunks(part)
                        .map(|c| Ok(Bytes::copy_from_slice(c)))
                        .collect();
                    files::Body::stream(futures::stream::iter(parts))
                } else {
                    files::Body::from(body.clone())
                };
                let handle = self.files.put(&tx_ctx, FIELD, put_body).await?;
                let info = self.files.stat(&tx_ctx, &handle).await?;
                let back = self.files.read(&tx_ctx, &handle).await?;
                self.files.delete(&tx_ctx, &handle).await?;
                let gone = match self.files.read(&tx_ctx, &handle).await {
                    Err(e) => e.is_not_found(),
                    Ok(_) => false,
                };
                let mod_time_ms = info
                    .mod_time
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as i64;
                Ok::<_, Status>(v1::AttachFileResponse {
                    handle,
                    stat_size: info.size as i64,
                    mod_time_ms,
                    read_back_equal: back == body,
                    not_found_after_delete: gone,
                    refused: String::new(),
                })
            },
        )
        .await?;
        Ok(Response::new(resp))
    }
}
