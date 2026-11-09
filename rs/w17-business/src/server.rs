//! Serving business methods: the registry, the middleware, the in-process
//! channel.
//!
//! The author implements the tonic traits the build helper generated and
//! registers the generated servers:
//!
//! ```ignore
//! registry.register(CheckoutServiceServer::new(Checkout { … }))?;
//! ```
//!
//! The registry refuses a service the manifest does not list, or one
//! registered twice; a missing method does not compile.
//!
//! **The middleware** is one `tower` layer over the served router, and it
//! needs none of the author's types. It is the one place that sees every
//! call:
//!
//! - it builds the [`Ctx`] (incoming metadata, `grpc-timeout`, the method)
//!   and puts it in the request's extensions, where [`crate::ctx`] finds it
//!   — or keeps the caller's on a [`SelfChannel`] call;
//! - it ends a call whose deadline passed (`DEADLINE_EXCEEDED`; the handler's
//!   future is dropped, which is Rust's cancellation) and cancels the
//!   context when the call ends or the caller goes away;
//! - after an `OK` unary call the manifest lists an emit or an invalidate
//!   for, it decodes the request and the response against the descriptor set
//!   and publishes the event — at once, or after the root commit when the
//!   context carries an open transaction (dropped on rollback). Streaming
//!   methods carry no emit;
//! - a [`crate::for_user`] status leaves prefixed with the method; a panic
//!   answers `INTERNAL` `"<method>: internal error"` and is reported;
//! - the page position a forwarded storage call reported answers as the
//!   method's own response headers.
//!
//! **Compose your own methods through [`Registry::self_channel`]**, an
//! in-process channel into the served router with its middleware: a direct
//! call on your struct skips the emit, and nothing fails — exactly the Go
//! rule.

use std::{
    collections::{HashMap, HashSet},
    convert::Infallible,
    future::Future,
    panic::AssertUnwindSafe,
    pin::Pin,
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context as TaskContext, Poll},
    time::{Duration, Instant},
};

use bytes::{Buf, Bytes};
use futures::FutureExt as _;
use http::{HeaderMap, HeaderValue};
use http_body::{Body as HttpBody, Frame};
use http_body_util::{BodyExt, Full, StreamBody};
use prost_reflect::DescriptorPool;
use tokio::sync::Notify;
use tokio_util::sync::{CancellationToken, DropGuard};
use tonic::{Status, body::Body, server::NamedService, service::Routes};
use tower::ServiceExt as _;

use crate::{
    BootError,
    channel::{BoxError, set_metadata},
    ctx::{Ctx, HeaderSink},
    errors::finish_for_user,
    events::{self, Dispatcher, EnvelopeBuilder},
    manifest::{Emit, Invalidate},
    metadata::Metadata,
    observability::{self, ServerSpan},
    txscope::{self, Outcome},
};

type BoxFuture<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// A drain hook: called with a context bounded by the drain budget after the
/// server stopped serving.
pub(crate) type ShutdownHook = Box<dyn FnOnce(Ctx) -> BoxFuture<()> + Send>;

/// One served RPC, as the middleware sees it.
pub(crate) struct MethodRt {
    pub(crate) client_streaming: bool,
    pub(crate) server_streaming: bool,
    builder: Option<Arc<EnvelopeBuilder>>,
    invalidates: Vec<Invalidate>,
}

impl MethodRt {
    fn emits(&self) -> bool {
        self.builder.is_some() || !self.invalidates.is_empty()
    }
}

/// What the middleware shares across calls and the drain reads.
pub(crate) struct Shared {
    pub(crate) methods: HashMap<String, MethodRt>,
    pub(crate) dispatcher: Option<Arc<dyn Dispatcher>>,
    pub(crate) draining: AtomicBool,
    /// Cancels every handler still running (the drain's last step).
    pub(crate) kill: CancellationToken,
    inflight: AtomicUsize,
    idle: Notify,
}

impl Shared {
    pub(crate) fn inflight(&self) -> usize {
        self.inflight.load(Ordering::SeqCst)
    }

    /// True once no call is in flight, false after `timeout`.
    pub(crate) async fn wait_idle(&self, timeout: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.idle.notified();
            if self.inflight() == 0 {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return self.inflight() == 0;
            }
        }
    }
}

struct InflightGuard(Arc<Shared>);

impl InflightGuard {
    fn enter(shared: &Arc<Shared>) -> Self {
        shared.inflight.fetch_add(1, Ordering::SeqCst);
        InflightGuard(shared.clone())
    }
}

impl Drop for InflightGuard {
    fn drop(&mut self) {
        if self.0.inflight.fetch_sub(1, Ordering::SeqCst) == 1 {
            self.0.idle.notify_waiters();
        }
    }
}

/// What `register_business(cfg, registry, clients)` registers into.
pub struct Registry {
    pool: DescriptorPool,
    allowed: HashSet<String>,
    emits: HashMap<String, Emit>,
    invalidates: HashMap<String, Vec<Invalidate>>,
    routes: Option<Routes>,
    registered: Vec<String>,
    methods: HashMap<String, MethodRt>,
    shutdowns: Vec<ShutdownHook>,
    served: Arc<OnceLock<Served>>,
}

impl Registry {
    pub(crate) fn new(
        pool: DescriptorPool,
        services: &[String],
        emits: &[Emit],
        invalidates: &[Invalidate],
    ) -> Self {
        let mut invs: HashMap<String, Vec<Invalidate>> = HashMap::new();
        for i in invalidates {
            invs.entry(i.method.clone()).or_default().push(i.clone());
        }
        Self {
            pool,
            allowed: services
                .iter()
                .map(|s| s.trim_start_matches('.').to_string())
                .collect(),
            emits: emits
                .iter()
                .map(|e| (e.method.clone(), e.clone()))
                .collect(),
            invalidates: invs,
            routes: Some(Routes::default()),
            registered: Vec::new(),
            methods: HashMap::new(),
            shutdowns: Vec::new(),
            served: Arc::new(OnceLock::new()),
        }
    }

    /// Serve `svc` — a server tonic generated (`NoteServiceServer::new(…)`).
    ///
    /// Refused ([`BootError`], which stops the boot) when the manifest does
    /// not list the service as one of this tier's, or it is registered
    /// twice.
    pub fn register<S>(&mut self, svc: S) -> Result<(), BootError>
    where
        S: tower_service::Service<
                http::Request<Body>,
                Response = http::Response<Body>,
                Error = Infallible,
            > + NamedService
            + Clone
            + Send
            + Sync
            + 'static,
        S::Future: Send + 'static,
    {
        let name = S::NAME;
        if !self.allowed.contains(name) {
            let mut listed: Vec<&str> = self.allowed.iter().map(String::as_str).collect();
            listed.sort();
            let listed = if listed.is_empty() {
                "none".to_string()
            } else {
                listed.join(", ")
            };
            return Err(BootError(format!(
                "registry: {name} is not a business service of this tier (the manifest lists: {listed})"
            )));
        }
        if self.registered.iter().any(|r| r == name) {
            return Err(BootError(format!("registry: {name} is already registered")));
        }
        let desc = self.pool.get_service_by_name(name).ok_or_else(|| {
            BootError(format!(
                "registry: no service {name:?} in the descriptor set"
            ))
        })?;
        for m in desc.methods() {
            let path = format!("/{name}/{}", m.name());
            let builder = match self.emits.get(&path) {
                None => None,
                Some(e) => Some(Arc::new(
                    EnvelopeBuilder::new(&self.pool, e, m.input(), m.output())
                        .map_err(|err| BootError(format!("events: {path}: {err}")))?,
                )),
            };
            let invalidates = self.invalidates.get(&path).cloned().unwrap_or_default();
            let streaming = m.is_client_streaming() || m.is_server_streaming();
            if streaming && (builder.is_some() || !invalidates.is_empty()) {
                return Err(BootError(format!(
                    "registry: {path} is streaming and cannot emit an event"
                )));
            }
            self.methods.insert(
                path,
                MethodRt {
                    client_streaming: m.is_client_streaming(),
                    server_streaming: m.is_server_streaming(),
                    builder,
                    invalidates,
                },
            );
        }
        self.registered.push(name.to_string());
        let routes = self.routes.take().unwrap_or_default();
        self.routes = Some(routes.add_service(svc));
        Ok(())
    }

    /// An in-process channel into the served router, with its middleware:
    /// compose your own methods through it
    /// (`NoteServiceClient::new(registry.self_channel())`), never by calling
    /// your struct directly, or their events are skipped.
    ///
    /// It is Go's direct `s.self.Method(ctx, req)`: the callee gets the
    /// context you pass UNCHANGED — your incoming metadata, your transaction
    /// (it joins), and whatever you put on it. So pass a `paging::forward`
    /// context only to the storage call that returns the page, never here.
    pub fn self_channel(&self) -> SelfChannel {
        SelfChannel {
            served: self.served.clone(),
        }
    }

    /// A drain hook: called with a context bounded by the drain budget after
    /// the server stopped serving.
    pub fn register_shutdown<F, Fut>(&mut self, f: F)
    where
        F: FnOnce(Ctx) -> Fut + Send + 'static,
        Fut: Future<Output = ()> + Send + 'static,
    {
        self.shutdowns.push(Box::new(move |ctx| Box::pin(f(ctx))));
    }

    /// The services registered, by full name.
    pub fn registered(&self) -> &[String] {
        &self.registered
    }

    /// The gRPC paths the registered services serve.
    pub fn paths(&self) -> impl Iterator<Item = &str> {
        self.methods.keys().map(String::as_str)
    }

    pub(crate) fn take_shutdowns(&mut self) -> Vec<ShutdownHook> {
        std::mem::take(&mut self.shutdowns)
    }

    /// The served router: the registered services plus `extra` (health,
    /// reflection), behind the middleware.
    pub(crate) fn finish(
        &mut self,
        dispatcher: Option<Arc<dyn Dispatcher>>,
        extra: impl FnOnce(Routes) -> Routes,
    ) -> Served {
        let routes = extra(self.routes.take().unwrap_or_default()).prepare();
        let shared = Arc::new(Shared {
            methods: std::mem::take(&mut self.methods),
            dispatcher,
            draining: AtomicBool::new(false),
            kill: CancellationToken::new(),
            inflight: AtomicUsize::new(0),
            idle: Notify::new(),
        });
        let served = Served { routes, shared };
        let _ = self.served.set(served.clone());
        served
    }
}

/// The served router behind the middleware.
#[derive(Clone)]
pub(crate) struct Served {
    routes: Routes,
    pub(crate) shared: Arc<Shared>,
}

/// `grpc-timeout` as a duration (`<digits><unit>`, units H M S m u n).
pub(crate) fn parse_timeout(v: &HeaderValue) -> Option<Duration> {
    let s = v.to_str().ok()?;
    if s.len() < 2 || s.len() > 9 {
        return None;
    }
    let (digits, unit) = s.split_at(s.len() - 1);
    let n: u64 = digits.parse().ok()?;
    Some(match unit {
        "H" => Duration::from_secs(n * 3600),
        "M" => Duration::from_secs(n * 60),
        "S" => Duration::from_secs(n),
        "m" => Duration::from_millis(n),
        "u" => Duration::from_micros(n),
        "n" => Duration::from_nanos(n),
        _ => return None,
    })
}

/// The message of a unary request or response body: one gRPC frame
/// (uncompressed).
pub(crate) fn frame_message(mut b: Bytes) -> Result<Bytes, String> {
    if b.len() < 5 {
        return Err("no gRPC frame".into());
    }
    if b[0] != 0 {
        return Err("a compressed gRPC frame".into());
    }
    let len = u32::from_be_bytes([b[1], b[2], b[3], b[4]]) as usize;
    b.advance(5);
    if b.len() < len {
        return Err("a truncated gRPC frame".into());
    }
    Ok(b.split_to(len))
}

fn panic_text(p: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = p.downcast_ref::<&str>() {
        (*s).to_string()
    } else if let Some(s) = p.downcast_ref::<String>() {
        s.clone()
    } else {
        "a panic".into()
    }
}

/// Everything a call holds until its response body ends: the context's
/// cancellation (cancelled when the call ends or the caller goes away), the
/// in-flight count, the server span.
struct CallGuards {
    _cancel: Option<DropGuard>,
    _inflight: Option<InflightGuard>,
    span: Option<ServerSpan>,
}

impl Drop for CallGuards {
    fn drop(&mut self) {
        if let Some(s) = self.span.take() {
            s.end(None);
        }
    }
}

/// The response body of a served call: ends with a status trailer when the
/// call's deadline passes (`DEADLINE_EXCEEDED`) or the drain cancels it
/// (`UNAVAILABLE`) while the body is still being written — a
/// server-streaming method's stream.
struct ServerBody {
    inner: Body,
    deadline: Option<Pin<Box<tokio::time::Sleep>>>,
    kill: Option<Pin<Box<tokio_util::sync::WaitForCancellationFutureOwned>>>,
    ended: bool,
    _guards: CallGuards,
}

fn status_trailers(st: Status) -> HeaderMap {
    st.into_http::<()>().into_parts().0.headers
}

impl HttpBody for ServerBody {
    type Data = Bytes;
    type Error = Status;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, Status>>> {
        let this = self.get_mut();
        if this.ended {
            return Poll::Ready(None);
        }
        if let Some(d) = this.deadline.as_mut()
            && d.as_mut().poll(cx).is_ready()
        {
            this.ended = true;
            return Poll::Ready(Some(Ok(Frame::trailers(status_trailers(
                Status::deadline_exceeded("context deadline exceeded"),
            )))));
        }
        if let Some(k) = this.kill.as_mut()
            && k.as_mut().poll(cx).is_ready()
        {
            this.ended = true;
            return Poll::Ready(Some(Ok(Frame::trailers(status_trailers(
                Status::unavailable("server is shutting down"),
            )))));
        }
        let f = Pin::new(&mut this.inner).poll_frame(cx);
        if let Poll::Ready(None) = f {
            this.ended = true;
        }
        f
    }

    fn is_end_stream(&self) -> bool {
        // A trailers-only answer has an empty body: HTTP/2 must end the
        // stream on its HEADERS frame, or a client reads the status as
        // response headers and waits for trailers.
        self.ended || self.inner.is_end_stream()
    }
}

impl Served {
    /// A call from the network.
    pub(crate) async fn serve_wire<B>(self, req: http::Request<B>) -> http::Response<Body>
    where
        B: HttpBody<Data = Bytes> + Send + 'static,
        B::Error: Into<BoxError>,
    {
        self.handle(req.map(Body::new), true).await
    }

    async fn handle(self, mut req: http::Request<Body>, wire: bool) -> http::Response<Body> {
        let path = req.uri().path().to_string();
        let shared = self.shared.clone();
        let Some(method) = shared.methods.get(&path) else {
            // Health, reflection, or a method nobody serves (Unimplemented).
            return self
                .routes
                .oneshot(req)
                .await
                .unwrap_or_else(|e| match e {});
        };
        if wire && shared.draining.load(Ordering::SeqCst) {
            // Shutting down: refuse new work with the one code a client
            // retries elsewhere, rather than start what the drain may cut off.
            return Status::unavailable("server is shutting down").into_http();
        }

        let mut guards = CallGuards {
            _cancel: None,
            _inflight: None,
            span: None,
        };
        let mut deadline: Option<Instant> = None;
        let mut sink: Option<HeaderSink> = None;
        let ctx = if wire {
            let timeout = req.headers().get("grpc-timeout").and_then(parse_timeout);
            deadline = timeout.map(|t| Instant::now() + t);
            let token = CancellationToken::new();
            let s: HeaderSink = Arc::new(Mutex::new(Metadata::new()));
            sink = Some(s.clone());
            let ctx = Ctx::served(
                Metadata::from_headers(req.headers()),
                deadline,
                token.clone(),
                &path,
                Some(s),
                method.client_streaming && method.server_streaming,
            );
            guards._cancel = Some(token.drop_guard());
            guards._inflight = Some(InflightGuard::enter(&shared));
            let (ctx, span) = observability::start_server_span(ctx, req.headers(), &path);
            guards.span = Some(span);
            req.extensions_mut().insert(ctx.clone());
            ctx
        } else {
            match req.extensions().get::<Ctx>() {
                Some(c) => c.clone(),
                None => {
                    return Status::failed_precondition(
                        "w17_business: a composed call without a context",
                    )
                    .into_http();
                }
            }
        };

        // An emitting method's request is kept: its event is built from it.
        let mut req_msg = None;
        if method.emits() {
            let (parts, body) = req.into_parts();
            let bytes = match body.collect().await {
                Ok(b) => b.to_bytes(),
                Err(e) => return e.into_http(),
            };
            req_msg = frame_message(bytes.clone()).ok();
            req = http::Request::from_parts(parts, Body::new(Full::new(bytes)));
        }

        let call = AssertUnwindSafe(self.routes.clone().oneshot(req)).catch_unwind();
        let outcome = if wire {
            let sleep = async {
                match deadline {
                    Some(d) => tokio::time::sleep_until(d.into()).await,
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                r = call => r,
                _ = sleep => return Status::deadline_exceeded("context deadline exceeded").into_http(),
                _ = shared.kill.cancelled() => return Status::unavailable("server is shutting down").into_http(),
            }
        } else {
            call.await
        };
        let mut resp = match outcome {
            Ok(Ok(resp)) => resp,
            Ok(Err(e)) => match e {},
            Err(p) => {
                observability::report_error(&format!("PANIC {path}: {}", panic_text(p.as_ref())));
                if let Some(s) = guards.span.take() {
                    s.end(Some("panic"));
                }
                return Status::internal(format!("{path}: internal error")).into_http();
            }
        };

        if wire && let Some(st) = Status::from_header_map(resp.headers()) {
            // A trailers-only answer: the handler returned a status.
            if let Some(s) = guards.span.take() {
                s.end(Some(st.message()));
            }
            if let Some(user) = finish_for_user(&st, &path) {
                return user.into_http();
            }
        }

        if method.emits() && Status::from_header_map(resp.headers()).is_none() {
            let (parts, body) = resp.into_parts();
            let collected = match body.collect().await {
                Ok(c) => c,
                Err(st) => return st.into_http(),
            };
            let trailers = collected.trailers().cloned();
            let data = collected.to_bytes();
            let ok = trailers
                .as_ref()
                .and_then(|t| t.get("grpc-status"))
                .map(|v| v.as_bytes() == b"0")
                .unwrap_or(false);
            if ok && let (Some(req_msg), Ok(resp_msg)) = (req_msg, frame_message(data.clone())) {
                emit(&ctx, &shared, &path, method, req_msg, resp_msg).await;
            }
            let mut frames: Vec<Result<Frame<Bytes>, Status>> = vec![Ok(Frame::data(data))];
            if let Some(t) = trailers {
                frames.push(Ok(Frame::trailers(t)));
            }
            resp = http::Response::from_parts(
                parts,
                Body::new(StreamBody::new(futures::stream::iter(frames))),
            );
        }

        if let Some(sink) = sink {
            let md = sink.lock().unwrap_or_else(|e| e.into_inner()).clone();
            if !md.is_empty() {
                // The page position a forwarded storage call reported
                // answers as this method's own.
                set_metadata(resp.headers_mut(), &md);
            }
        }
        if !wire || Status::from_header_map(resp.headers()).is_some() {
            return resp;
        }
        resp.map(|inner| {
            Body::new(ServerBody {
                inner,
                deadline: deadline.map(|d| Box::pin(tokio::time::sleep_until(d.into()))),
                kill: Some(Box::pin(shared.kill.clone().cancelled_owned())),
                ended: false,
                _guards: guards,
            })
        })
    }
}

/// The Go emit wrapper: after success, publish — or, inside a transaction,
/// park until the root commits (dropped on rollback). The emit goes first,
/// the invalidates after it, as the Go helper orders them.
async fn emit(
    ctx: &Ctx,
    shared: &Arc<Shared>,
    path: &str,
    method: &MethodRt,
    req: Bytes,
    resp: Bytes,
) {
    let deferred = ctx.without_cancel();
    let builder = method.builder.clone();
    let invalidates = method.invalidates.clone();
    let dispatcher = shared.dispatcher.clone();
    let what = path.to_string();
    let publish: txscope::Hook = Box::pin(async move {
        if let Some(b) = builder {
            match b.build(&deferred, &req, &resp) {
                Ok(payload) => {
                    events::fire(
                        dispatcher.as_deref(),
                        &b.emit().channel,
                        &b.emit().topic,
                        payload,
                        &what,
                    )
                    .await;
                }
                // an emit never fails the RPC
                Err(e) => {
                    observability::report_error(&format!("eventbus: build the {what} event: {e}"))
                }
            }
        }
        for inv in &invalidates {
            for payload in events::invalidate_events(inv, &deferred) {
                events::fire(
                    dispatcher.as_deref(),
                    &inv.channel,
                    &inv.topic,
                    payload,
                    &what,
                )
                .await;
            }
        }
        Ok(())
    });
    match txscope::after_commit(ctx, publish) {
        (Outcome::Deferred, _) => {}
        (Outcome::Discarded, _) => observability::report_error(
            "txregistry: dropped an announcement made after its in-process transaction rolled back",
        ),
        (_, Some(hook)) => {
            // The write behind it has landed: a caller that hangs up now must
            // not take the event with it.
            let _ = txscope::shielded(hook).await;
        }
        (_, None) => {}
    }
}

/// The in-process channel [`Registry::self_channel`] returns: a call through
/// it runs the served method with its middleware, the caller's context
/// travelling in the request unchanged.
#[derive(Clone)]
pub struct SelfChannel {
    served: Arc<OnceLock<Served>>,
}

impl tower_service::Service<http::Request<Body>> for SelfChannel {
    type Response = http::Response<Body>;
    type Error = BoxError;
    type Future = BoxFuture<Result<http::Response<Body>, BoxError>>;

    fn poll_ready(&mut self, _cx: &mut TaskContext<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, req: http::Request<Body>) -> Self::Future {
        let served = self.served.get().cloned();
        Box::pin(async move {
            if req.extensions().get::<Ctx>().is_none() {
                return Ok(Status::failed_precondition(
                    "w17_business: a call without a context — build the request with ctx.request(msg), so the \
                     caller's principal, deadline and transaction go with it",
                )
                .into_http());
            }
            let Some(served) = served else {
                return Ok(
                    Status::unavailable("w17_business: the server is not serving yet").into_http(),
                );
            };
            Ok(served.handle(req, false).await)
        })
    }
}
