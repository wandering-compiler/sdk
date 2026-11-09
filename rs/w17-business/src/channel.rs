//! The channels a tier calls through: storage (and any service the manifest
//! lists there) and the business companion.
//!
//! A channel is a `tower` service under the author's tonic clients
//! (`OrderMutationClient::new(clients.storage())`). Every call does what Go's
//! generated inter-tier client does (sdk/go/core/grpcx `DialOpts`):
//!
//! - it takes the context from the request ([`Ctx::request`]) and REFUSES a
//!   request without one (`FAILED_PRECONDITION`), so a call that forgot its
//!   context fails instead of dropping the principal silently;
//! - relays the verified principal ([`principal::forward_to_outgoing`]) and
//!   sends the context's outgoing metadata (transaction routing, paging the
//!   author forwarded) and its deadline (`grpc-timeout`); a context already
//!   done is refused before anything is sent, and cancelling the context (or
//!   its deadline passing) ends the call;
//! - injects W3C trace context when OpenTelemetry is on;
//! - retries `UNAVAILABLE` and nothing else: 3 attempts, backoff 0.1 s × 2 up
//!   to 1 s, under a retry throttle (100 tokens, 0.1 per success) —
//!   `grpcclient.DefaultServiceConfig`. A call is never retried once the
//!   server's response headers arrived, and a call whose request is a stream
//!   is never retried;
//! - keeps the connection alive (ping every 30 s, 10 s timeout, also without
//!   calls) and dials TLS per `W17_INTERNAL_TLS`.
//!
//! The companion channel is the same with two differences: ONE attempt per
//! call (the egress runtime already retried what is safe to retry, and a
//! `Put` stream cannot be replayed), and none of the storage-transaction or
//! paging headers (`w17-tx-id`, `w17-conn-id`, `x-w17-paging-*`) — nor a
//! page position relayed back.

use std::{
    collections::HashSet,
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    task::{Context as TaskContext, Poll},
    time::Duration,
};

use bytes::Bytes;
use http::{HeaderMap, HeaderName, HeaderValue};
use http_body::{Body as HttpBody, Frame};
use http_body_util::{BodyExt, Full};
use tonic::{Status, body::Body, transport::Channel};
use tower::ServiceExt;
use tower_service::Service as _;

use crate::{
    ctx::{Ctx, CtxError},
    metadata::Metadata,
    observability, paging, principal,
};

pub(crate) type BoxError = Box<dyn std::error::Error + Send + Sync>;
type BoxFuture<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// grpcclient.DefaultServiceConfig: three attempts, 0.1 s × 2 up to 1 s.
const MAX_ATTEMPTS: u32 = 3;
const INITIAL_BACKOFF: Duration = Duration::from_millis(100);
const MAX_BACKOFF: Duration = Duration::from_secs(1);
const THROTTLE_MAX: f64 = 100.0;
const THROTTLE_RATIO: f64 = 0.1;

/// grpcclient.DefaultClientKeepalive.
pub(crate) const KEEPALIVE_TIME: Duration = Duration::from_secs(30);
pub(crate) const KEEPALIVE_TIMEOUT: Duration = Duration::from_secs(10);

const NO_CONTEXT: &str = "w17_business: a call without a context — build the request with ctx.request(msg), \
                          so the caller's principal, deadline and transaction go with it";

/// Never sent to the companion.
const COMPANION_STRIPPED: &[&str] = &["w17-tx-id", "w17-conn-id"];
const COMPANION_STRIPPED_PREFIX: &str = "x-w17-paging-";

fn companion_stripped(k: &str) -> bool {
    COMPANION_STRIPPED.contains(&k) || k.starts_with(COMPANION_STRIPPED_PREFIX)
}

pub(crate) struct Outbound {
    channel: Option<Channel>,
    refusal: Option<String>,
    companion: bool,
    /// The gRPC paths whose request is a stream: never buffered, never
    /// retried.
    streaming_requests: HashSet<String>,
    tokens: Mutex<f64>,
}

impl Outbound {
    pub(crate) fn new(
        channel: Channel,
        companion: bool,
        streaming_requests: HashSet<String>,
    ) -> Self {
        Self {
            channel: Some(channel),
            refusal: None,
            companion,
            streaming_requests,
            tokens: Mutex::new(THROTTLE_MAX),
        }
    }

    /// A channel that answers every call `FAILED_PRECONDITION` with `why`
    /// (the storage channel of a domain with no storage tier).
    pub(crate) fn refusing(why: String) -> Self {
        Self {
            channel: None,
            refusal: Some(why),
            companion: false,
            streaming_requests: HashSet::new(),
            tokens: Mutex::new(THROTTLE_MAX),
        }
    }

    fn throttle_failure(&self) -> bool {
        let mut t = self.tokens.lock().unwrap_or_else(|e| e.into_inner());
        *t = (*t - 1.0).max(0.0);
        *t > THROTTLE_MAX / 2.0
    }

    fn throttle_success(&self) {
        let mut t = self.tokens.lock().unwrap_or_else(|e| e.into_inner());
        *t = (*t + THROTTLE_RATIO).min(THROTTLE_MAX);
    }

    fn stamp(&self, ctx: &Ctx, headers: &mut HeaderMap) {
        let out = principal::forward_to_outgoing(ctx);
        let md = if self.companion {
            out.outgoing().without(companion_stripped)
        } else {
            out.outgoing().clone()
        };
        set_metadata(headers, &md);
        if self.companion {
            let keys: Vec<HeaderName> = headers
                .keys()
                .filter(|k| companion_stripped(k.as_str()))
                .cloned()
                .collect();
            for k in keys {
                headers.remove(k);
            }
        }
        observability::inject_trace(ctx, headers);
        headers.remove("grpc-timeout");
        if let Some(rem) = ctx.time_remaining() {
            headers.insert("grpc-timeout", encode_timeout(rem));
        }
    }

    async fn send(&self, ctx: &Ctx, req: http::Request<Body>) -> Sent {
        let Some(mut ch) = self.channel.clone() else {
            return Sent::Response(
                Status::failed_precondition(self.refusal.clone().unwrap_or_default()).into_http(),
            );
        };
        let call = async move {
            let ch = ch.ready().await?;
            ch.call(req).await
        };
        tokio::select! {
            r = call => match r {
                Ok(resp) => Sent::Response(resp),
                Err(e) => Sent::Transport(e.to_string()),
            },
            why = ctx.done() => Sent::Done(why),
        }
    }

    fn finish(&self, ctx: &Ctx, resp: http::Response<Body>) -> http::Response<Body> {
        if !self.companion {
            paging::relay_position(ctx, resp.headers());
        }
        let ctx = ctx.clone();
        resp.map(|b| {
            Body::new(DoneBody {
                inner: b,
                done: Some(Box::pin(async move { ctx.done().await })),
            })
        })
    }

    async fn call(
        self: Arc<Self>,
        req: http::Request<Body>,
    ) -> Result<http::Response<Body>, BoxError> {
        let Some(ctx) = req.extensions().get::<Ctx>().cloned() else {
            return Ok(Status::failed_precondition(NO_CONTEXT).into_http());
        };
        if let Some(why) = ctx.err() {
            return Ok(why.status().into_http());
        }
        let (mut parts, body) = req.into_parts();
        parts.extensions = http::Extensions::new();
        self.stamp(&ctx, &mut parts.headers);
        let path = parts.uri.path().to_string();
        if self.companion || self.streaming_requests.contains(&path) {
            return Ok(
                match self
                    .send(&ctx, http::Request::from_parts(parts, body))
                    .await
                {
                    Sent::Response(resp) => self.finish(&ctx, resp),
                    Sent::Transport(e) => Status::unavailable(e).into_http(),
                    Sent::Done(why) => why.status().into_http(),
                },
            );
        }
        // A unary or server-streaming request is one message, already
        // encoded: buffered, so a retry can send it again.
        let bytes = match body.collect().await {
            Ok(b) => b.to_bytes(),
            Err(e) => {
                return Ok(Status::internal(format!("{path}: encode the request: {e}")).into_http());
            }
        };
        let mut backoff = INITIAL_BACKOFF;
        let mut attempt = 1;
        loop {
            let mut req = http::Request::new(Body::new(Full::new(bytes.clone())));
            *req.method_mut() = parts.method.clone();
            *req.uri_mut() = parts.uri.clone();
            *req.version_mut() = parts.version;
            *req.headers_mut() = parts.headers.clone();
            let retryable = match self.send(&ctx, req).await {
                Sent::Done(why) => return Ok(why.status().into_http()),
                Sent::Transport(e) => Err(Status::unavailable(e)),
                Sent::Response(resp) => {
                    if trailers_only_unavailable(resp.headers()) {
                        Ok(resp)
                    } else {
                        self.throttle_success();
                        return Ok(self.finish(&ctx, resp));
                    }
                }
            };
            let allowed = self.throttle_failure();
            if attempt >= MAX_ATTEMPTS || !allowed {
                return Ok(match retryable {
                    Ok(resp) => self.finish(&ctx, resp),
                    Err(st) => st.into_http(),
                });
            }
            attempt += 1;
            let pause = backoff.mul_f64(fastrand::f64());
            backoff = (backoff * 2).min(MAX_BACKOFF);
            tokio::select! {
                _ = tokio::time::sleep(pause) => {}
                why = ctx.done() => return Ok(why.status().into_http()),
            }
        }
    }
}

enum Sent {
    Response(http::Response<Body>),
    Transport(String),
    Done(CtxError),
}

/// A trailers-only `UNAVAILABLE`: the server answered nothing else, so the
/// call may be sent again (gRPC A6: it was never committed).
fn trailers_only_unavailable(headers: &HeaderMap) -> bool {
    headers.get("grpc-status").and_then(|v| v.to_str().ok()) == Some("14")
}

/// `rem` as a `grpc-timeout` value: milliseconds, or seconds past what eight
/// digits of milliseconds hold.
pub(crate) fn encode_timeout(rem: Duration) -> HeaderValue {
    let ms = rem.as_millis().max(1);
    let v = if ms < 100_000_000 {
        format!("{ms}m")
    } else {
        format!("{}S", rem.as_secs().min(99_999_999))
    };
    HeaderValue::from_str(&v).expect("digits and a unit")
}

/// `md` onto `headers`: each of its keys replaces the header's values. A key
/// or value HTTP/2 cannot carry is dropped (and logged).
pub(crate) fn set_metadata(headers: &mut HeaderMap, md: &Metadata) {
    for (k, vs) in md.iter() {
        let Ok(name) = HeaderName::from_bytes(k.as_bytes()) else {
            observability::warn(&format!(
                "metadata key {k:?} is not a header name; not sent"
            ));
            continue;
        };
        headers.remove(&name);
        for v in vs {
            let bytes: Option<Vec<u8>> = v.chars().map(|c| u8::try_from(c as u32).ok()).collect();
            match bytes.and_then(|b| HeaderValue::from_bytes(&b).ok()) {
                Some(hv) => {
                    headers.append(name.clone(), hv);
                }
                None => observability::warn(&format!(
                    "metadata {k}: a value HTTP/2 cannot carry; not sent"
                )),
            }
        }
    }
}

/// A response body that ends with the context's status once the context is
/// done (its deadline passed, or it was cancelled) while the body is still
/// being read: the stream of a server-streaming call, the reply of a unary
/// one.
struct DoneBody {
    inner: Body,
    done: Option<BoxFuture<CtxError>>,
}

impl HttpBody for DoneBody {
    type Data = Bytes;
    type Error = Status;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, Status>>> {
        let this = self.get_mut();
        if let Some(done) = this.done.as_mut()
            && let Poll::Ready(why) = done.as_mut().poll(cx)
        {
            this.done = None;
            return Poll::Ready(Some(Err(why.status())));
        }
        let frame = Pin::new(&mut this.inner).poll_frame(cx);
        if let Poll::Ready(None) = frame {
            this.done = None;
        }
        frame
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }
}

macro_rules! channel_service {
    ($ty:ident) => {
        impl tower_service::Service<http::Request<Body>> for $ty {
            type Response = http::Response<Body>;
            type Error = BoxError;
            type Future = BoxFuture<Result<http::Response<Body>, BoxError>>;

            fn poll_ready(&mut self, _cx: &mut TaskContext<'_>) -> Poll<Result<(), Self::Error>> {
                Poll::Ready(Ok(()))
            }

            fn call(&mut self, req: http::Request<Body>) -> Self::Future {
                Box::pin(self.0.clone().call(req))
            }
        }
    };
}

/// The channel to the storage tier: every storage service the manifest lists
/// is reached through it (`OrderMutationClient::new(clients.storage())`).
/// One connection, dialled lazily, shared by every client built on it.
#[derive(Clone)]
pub struct StorageChannel(pub(crate) Arc<Outbound>);

/// The channel to the business companion: the egress clients
/// (`QuotesApiClient::new(clients.egress("quotes")?)`) and the file stores.
#[derive(Clone)]
pub struct CompanionChannel(pub(crate) Arc<Outbound>);

channel_service!(StorageChannel);
channel_service!(CompanionChannel);
