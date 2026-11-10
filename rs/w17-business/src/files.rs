//! File storage: the domain's upload connections, served by the business
//! companion.
//!
//! A Go business tier stores and reads files through `sdk/go/lib/kvfs`
//! in-process; this tier calls the same Go code in the companion over
//! `w17.common.files.W17Files`, so a file is laid out, named and read back
//! exactly as in Go. `clients.store("app-exports")?` is one connection:
//!
//! ```ignore
//! let store = clients.store("app-exports")?;
//! let handle = store.put(&ctx, "app.example.NoteExport.file", body).await?; // kvfs.Put
//! let data = store.read(&ctx, &handle).await?;                              // whole object
//! let mut chunks = store.open(&ctx, &handle).await?;                        // streamed
//! let info = store.stat(&ctx, &handle).await?;                              // size, mod_time
//! store.delete(&ctx, &handle).await?;                                       // writable only
//! ```
//!
//! The handle is the value the field's column holds — byte for byte what
//! Go's `kvfs.Put` returns. A file is not part of a storage transaction (as
//! in Go): a file put inside one that then rolls back stays, so delete it
//! yourself when the unit of work fails.

use std::{
    fmt,
    pin::Pin,
    sync::{Arc, Mutex},
    task::{Context as TaskContext, Poll},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use bytes::{Bytes, BytesMut};
use futures::{Stream, StreamExt, stream::BoxStream};
use tokio::sync::Notify;
use tonic::Status;

use crate::{
    channel::{BoxError, CompanionChannel},
    ctx::Ctx,
    errors::error_info,
    manifest::{FILES_SERVICE, FileConnection, FilePlacement},
    pb::w17::common::files::{
        DeleteRequest, OpenRequest, PutEnd, PutHeader, PutRequest, StatRequest, put_request::Part,
        w17_files_client::W17FilesClient,
    },
};

pub const SERVICE: &str = FILES_SERVICE;
/// The ErrorInfo domain of the companion's file errors.
pub const DOMAIN: &str = "w17.files";
/// The size of the chunks `put` sends.
pub const CHUNK_SIZE: usize = 64 << 10;

/// True for the companion's "no stored file has this handle"
/// (`FILE_NOT_FOUND`) — Go's `errors.Is(err, kvfs.ErrNotFound)`.
pub fn is_not_found(st: &Status) -> bool {
    error_info(st).is_some_and(|i| i.domain == DOMAIN && i.reason == "FILE_NOT_FOUND")
}

/// What a file call failed with.
#[derive(Debug)]
pub enum Error {
    /// The companion's status (a refusal, `FILE_NOT_FOUND`, a transport
    /// failure), unchanged.
    Status(Status),
    /// The body given to [`FileStore::put`] failed; nothing was stored.
    Body(BoxError),
    /// Refused by the SDK before anything was sent.
    Refused(String),
}

impl Error {
    /// The companion's status, when that is what failed.
    pub fn status(&self) -> Option<&Status> {
        match self {
            Error::Status(s) => Some(s),
            _ => None,
        }
    }

    /// [`is_not_found`] of the companion's status.
    pub fn is_not_found(&self) -> bool {
        self.status().is_some_and(is_not_found)
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Status(s) => f.write_str(s.message()),
            Error::Body(e) => write!(f, "{e}"),
            Error::Refused(m) => f.write_str(m),
        }
    }
}

impl std::error::Error for Error {}

impl From<Error> for Status {
    fn from(e: Error) -> Self {
        match e {
            Error::Status(s) => s,
            Error::Refused(m) => Status::failed_precondition(m),
            Error::Body(e) => Status::internal(format!("files: the body of a put failed: {e}")),
        }
    }
}

impl From<Status> for Error {
    fn from(s: Status) -> Self {
        Error::Status(s)
    }
}

/// What [`FileStore::put`] stores: bytes, or a stream of byte chunks (any
/// sizes: they are re-chunked to 64 KiB).
pub enum Body {
    Bytes(Bytes),
    Stream(BoxStream<'static, Result<Bytes, BoxError>>),
}

impl Body {
    /// A body read from `s`; an `Err` item fails the put with that error and
    /// stores nothing.
    pub fn stream<S, B, E>(s: S) -> Self
    where
        S: Stream<Item = Result<B, E>> + Send + 'static,
        B: Into<Bytes>,
        E: Into<BoxError>,
    {
        Body::Stream(s.map(|r| r.map(Into::into).map_err(Into::into)).boxed())
    }
}

impl From<Bytes> for Body {
    fn from(b: Bytes) -> Self {
        Body::Bytes(b)
    }
}

impl From<Vec<u8>> for Body {
    fn from(b: Vec<u8>) -> Self {
        Body::Bytes(b.into())
    }
}

impl From<&'static [u8]> for Body {
    fn from(b: &'static [u8]) -> Self {
        Body::Bytes(Bytes::from_static(b))
    }
}

/// A stored object's size and modification time (Go's `kvfs.Info`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FileInfo {
    pub size: u64,
    pub mod_time: SystemTime,
}

/// The placement of `field`: its full proto name, or `Message.field` when
/// that names exactly one placement.
pub fn resolve_field<'a>(
    field: &str,
    placements: &'a [FilePlacement],
) -> Result<&'a FilePlacement, Error> {
    let name = field.trim_start_matches('.');
    if let Some(p) = placements.iter().find(|p| p.field == name) {
        return Ok(p);
    }
    let suffix = format!(".{name}");
    let matches: Vec<&FilePlacement> = if name.contains('.') {
        placements
            .iter()
            .filter(|p| p.field.ends_with(&suffix))
            .collect()
    } else {
        Vec::new()
    };
    match matches.as_slice() {
        [one] => Ok(one),
        [] => {
            let mut known: Vec<&str> = placements.iter().map(|p| p.field.as_str()).collect();
            known.sort();
            let known = if known.is_empty() {
                "none".to_string()
            } else {
                known.join(", ")
            };
            Err(Error::Refused(format!(
                "files: {field:?} is not a field this tier writes files for (have: {known})"
            )))
        }
        many => {
            let mut names: Vec<&str> = many.iter().map(|p| p.field.as_str()).collect();
            names.sort();
            Err(Error::Refused(format!(
                "files: {field:?} is ambiguous ({})",
                names.join(", ")
            )))
        }
    }
}

/// One upload connection's store, as the companion serves it.
#[derive(Clone)]
pub struct FileStore {
    client: W17FilesClient<CompanionChannel>,
    connection: FileConnection,
    placements: Arc<Vec<FilePlacement>>,
}

impl FileStore {
    /// `placements` are every placement of the manifest.
    pub(crate) fn new(
        channel: CompanionChannel,
        connection: FileConnection,
        placements: Arc<Vec<FilePlacement>>,
    ) -> Self {
        Self {
            client: W17FilesClient::new(channel),
            connection,
            placements,
        }
    }

    /// The connection's name.
    pub fn name(&self) -> &str {
        &self.connection.name
    }

    /// True when this tier writes the connection (`upload.writer:
    /// BUSINESS`); one the gateway writes is read-only here.
    pub fn writable(&self) -> bool {
        self.connection.writable
    }

    fn require_writable(&self, what: &str) -> Result<(), Error> {
        if self.writable() {
            return Ok(());
        }
        Err(Error::Refused(format!(
            "files: {what} on {}: the gateway writes this connection; this tier only reads it",
            self.name()
        )))
    }

    /// Store `body` as a new object of `field` (full proto name, or
    /// `Message.field` when unambiguous) and return its handle. The body is
    /// sent in 64 KiB chunks. Nothing is stored unless the body is read to
    /// its end: a body that fails (its error is returned as it is,
    /// [`Error::Body`]), a cancelled context or one past its deadline leaves
    /// no object behind.
    pub async fn put(
        &self,
        ctx: &Ctx,
        field: &str,
        body: impl Into<Body>,
    ) -> Result<String, Error> {
        let p = resolve_field(field, &self.placements)?;
        if p.connection != self.name() {
            return Err(Error::Refused(format!(
                "files: {} is stored on {}, not on {}",
                p.field,
                p.connection,
                self.name()
            )));
        }
        self.require_writable(&format!("put {}", p.field))?;
        let failed = Arc::new(Failure::default());
        let requests = PutStream::new(p.field.clone(), body.into(), failed.clone());
        let mut client = self.client.clone();
        let call = client.put(ctx.request(requests));
        // Only the end marker commits: a body that failed never sends it,
        // and the call is dropped (reset) rather than half-closed.
        tokio::select! {
            biased;
            _ = failed.notify.notified() => Err(Error::Body(failed.take())),
            r = call => match failed.error.lock().unwrap_or_else(|e| e.into_inner()).take() {
                Some(e) => Err(Error::Body(e)),
                None => r.map(|resp| resp.into_inner().handle).map_err(Error::Status),
            },
        }
    }

    /// The stored object's bytes, as chunks of at most 64 KiB.
    pub async fn open(
        &self,
        ctx: &Ctx,
        handle: &str,
    ) -> Result<BoxStream<'static, Result<Bytes, Status>>, Error> {
        let req = OpenRequest {
            connection: self.name().to_string(),
            handle: handle.to_string(),
        };
        let stream = self
            .client
            .clone()
            .open(ctx.request(req))
            .await?
            .into_inner();
        Ok(stream
            .filter_map(|r| async move {
                match r {
                    Ok(m) if m.chunk.is_empty() => None,
                    Ok(m) => Some(Ok(Bytes::from(m.chunk))),
                    Err(e) => Some(Err(e)),
                }
            })
            .boxed())
    }

    /// The whole stored object, in memory.
    pub async fn read(&self, ctx: &Ctx, handle: &str) -> Result<Vec<u8>, Error> {
        let mut s = self.open(ctx, handle).await?;
        let mut out = Vec::new();
        while let Some(c) = s.next().await {
            out.extend_from_slice(&c?);
        }
        Ok(out)
    }

    pub async fn stat(&self, ctx: &Ctx, handle: &str) -> Result<FileInfo, Error> {
        let req = StatRequest {
            connection: self.name().to_string(),
            handle: handle.to_string(),
        };
        let r = self
            .client
            .clone()
            .stat(ctx.request(req))
            .await?
            .into_inner();
        let mod_time = r
            .mod_time
            .map(|t| {
                UNIX_EPOCH
                    + Duration::from_secs(t.seconds.max(0) as u64)
                    + Duration::from_nanos(t.nanos.max(0) as u64)
            })
            .unwrap_or(UNIX_EPOCH);
        Ok(FileInfo {
            size: r.size.max(0) as u64,
            mod_time,
        })
    }

    /// Remove a stored object; a missing one is not an error.
    pub async fn delete(&self, ctx: &Ctx, handle: &str) -> Result<(), Error> {
        self.require_writable("delete")?;
        let req = DeleteRequest {
            connection: self.name().to_string(),
            handle: handle.to_string(),
        };
        self.client.clone().delete(ctx.request(req)).await?;
        Ok(())
    }
}

#[derive(Default)]
pub(crate) struct Failure {
    error: Mutex<Option<BoxError>>,
    notify: Notify,
}

impl Failure {
    pub(crate) fn take(&self) -> BoxError {
        self.error
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
            .unwrap_or_else(|| "files: the body failed".into())
    }
}

/// The Put request stream: the header, the body re-chunked to 64 KiB, the
/// end marker with the size. A body that fails parks its error and never
/// ends the stream.
pub(crate) struct PutStream {
    header: Option<String>,
    body: Option<Body>,
    buf: BytesMut,
    size: u64,
    end_sent: bool,
    body_failed: bool,
    failed: Arc<Failure>,
}

impl PutStream {
    pub(crate) fn new(field: String, body: Body, failed: Arc<Failure>) -> Self {
        Self {
            header: Some(field),
            body: Some(body),
            buf: BytesMut::new(),
            size: 0,
            end_sent: false,
            body_failed: false,
            failed,
        }
    }

    fn chunk(&mut self, n: usize) -> PutRequest {
        let c = self.buf.split_to(n).freeze();
        self.size += c.len() as u64;
        PutRequest {
            part: Some(Part::Chunk(c.to_vec())),
        }
    }
}

impl Stream for PutStream {
    type Item = PutRequest;

    fn poll_next(self: Pin<&mut Self>, cx: &mut TaskContext<'_>) -> Poll<Option<PutRequest>> {
        let this = self.get_mut();
        if let Some(field) = this.header.take() {
            return Poll::Ready(Some(PutRequest {
                part: Some(Part::Header(PutHeader { field })),
            }));
        }
        loop {
            if this.buf.len() >= CHUNK_SIZE {
                return Poll::Ready(Some(this.chunk(CHUNK_SIZE)));
            }
            match this.body.as_mut() {
                Some(Body::Bytes(b)) => {
                    this.buf.extend_from_slice(b);
                    this.body = None;
                }
                Some(Body::Stream(s)) => match s.as_mut().poll_next(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(Some(Ok(part))) => this.buf.extend_from_slice(&part),
                    Poll::Ready(Some(Err(e))) => {
                        this.body = None;
                        *this.failed.error.lock().unwrap_or_else(|e| e.into_inner()) = Some(e);
                        this.failed.notify.notify_one();
                        this.body_failed = true;
                        this.buf.clear();
                        return Poll::Pending;
                    }
                    Poll::Ready(None) => this.body = None,
                },
                None => {
                    if !this.buf.is_empty() {
                        let n = this.buf.len();
                        return Poll::Ready(Some(this.chunk(n)));
                    }
                    if this.body_failed {
                        // Never end (see put).
                        return Poll::Pending;
                    }
                    if this.end_sent {
                        return Poll::Ready(None);
                    }
                    this.end_sent = true;
                    return Poll::Ready(Some(PutRequest {
                        part: Some(Part::End(PutEnd {
                            size: this.size as i64,
                        })),
                    }));
                }
            }
        }
    }
}
