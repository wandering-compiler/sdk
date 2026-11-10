//! Events: the envelope, its context data, and the brokers — byte-compatible
//! with sdk/go/lib/eventbus and the generated Go emit helpers.
//!
//! The envelope is the domain's events-lock message (`<pkg>.Event`): the
//! event in its `payload` oneof, `metadata` (50001) with the originating
//! request's metadata, `labels` (50002) with its broadcast labels. The
//! server middleware builds it from the manifest's emit — the method's
//! request and response decoded against the descriptor set — so an author's
//! service is plain tonic and a Go subscriber decodes what this module
//! publishes exactly as it decodes a Go emit.
//!
//! Brokers, as the Go adapters address them:
//!
//! - NATS JetStream: stream `<channel>` over subjects `<channel>.>` (limits
//!   retention, discard old, max age 7 days, no count limit — created or
//!   updated on first use), subject `<channel>.<topic>`;
//! - Redis Streams: `XADD <channel> MINID ~ <now-7d>-0 * topic <topic>
//!   payload <envelope bytes>`.
//!
//! Each publish is stamped with the business domain (the manifest's
//! `domain`), as a Go bus with a `Domain` stamps it: NATS header
//! `W17-Domain: <domain>`, Redis entry field `domain <domain>` after the
//! payload. Two domains that declare the same channel name share one stream
//! on a project's broker; a Go subscriber with a domain settles unread a
//! message stamped with another one. An empty domain stamps nothing.
//!
//! Emits are fire-and-forget: a failure is reported, never returned into
//! the RPC, and bounded by the publish timeout (2 s).

use std::{
    collections::{HashMap, HashSet},
    future::Future,
    pin::Pin,
    sync::Mutex,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use prost::Message as _;
use prost_reflect::{
    DescriptorPool, DynamicMessage, FieldDescriptor, Kind, MapKey, MessageDescriptor,
    ReflectMessage as _, Value,
};

use crate::{
    ctx::Ctx,
    manifest::{Emit, Invalidate},
    metadata::Metadata,
    observability,
    pb::w17::InvalidateEvent,
    principal::{LABEL_KEY_PREFIX, is_gateway_owned_key},
};

/// eventbus.DefaultStreamMaxAge.
pub const DEFAULT_STREAM_MAX_AGE: Duration = Duration::from_secs(7 * 24 * 3600);
/// eventbus.DefaultPublishTimeout.
pub const DEFAULT_PUBLISH_TIMEOUT: Duration = Duration::from_secs(2);
pub const MAX_CHANNEL_LEN: usize = 200;
pub const W17_INVALIDATE_TOPIC: &str = "w17.invalidate";

pub(crate) type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

// --- credentials (eventbus.isCredentialKey) ---------------------------------------

const CREDENTIAL_HEADERS: &[&str] = &[
    "authorization",
    "proxy-authorization",
    "cookie",
    "set-cookie",
    "x-w17-credential",
];
const CREDENTIAL_SEGMENTS: &[&str] = &[
    "auth",
    "authorization",
    "apikey",
    "cookie",
    "credential",
    "credentials",
    "jwt",
    "passwd",
    "password",
    "secret",
    "session",
    "token",
];
const GRPC_GATEWAY_PREFIX: &str = "grpcgateway-";
const TX_KEYS: &[&str] = &["w17-tx-id", "w17-conn-id"];

/// Whether a metadata key carries a request credential (stripped from an
/// event's envelope, scrubbed from a Sentry event).
pub fn is_credential_key(key: &str) -> bool {
    let low = key.trim().to_ascii_lowercase();
    let k = low.strip_prefix(GRPC_GATEWAY_PREFIX).unwrap_or(&low);
    if CREDENTIAL_HEADERS.contains(&k) {
        return true;
    }
    if is_gateway_owned_key(k) {
        return false;
    }
    let segs: Vec<&str> = k.split(['-', '_', '.']).filter(|s| !s.is_empty()).collect();
    segs.iter().enumerate().any(|(i, s)| {
        CREDENTIAL_SEGMENTS.contains(s) || (*s == "api" && segs.get(i + 1) == Some(&"key"))
    })
}

/// Stripped from an envelope: the transaction keys and credentials.
pub fn reserved_metadata_key(key: &str) -> bool {
    TX_KEYS.contains(&key.to_ascii_lowercase().as_str()) || is_credential_key(key)
}

// --- names (eventbus/names.go) ----------------------------------------------------

pub fn validate_channel(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("eventbus: invalid channel name: empty".into());
    }
    if name.len() > MAX_CHANNEL_LEN {
        return Err(format!(
            "eventbus: invalid channel name: {name:?} is longer than {MAX_CHANNEL_LEN} characters"
        ));
    }
    if let Some(c) = name
        .chars()
        .find(|c| !(c.is_ascii_alphanumeric() || *c == '_' || *c == '-'))
    {
        return Err(format!(
            "eventbus: invalid channel name: {name:?} contains {:?} (allowed: letters, digits, '_' and '-')",
            c.to_string()
        ));
    }
    let low = name.to_ascii_lowercase();
    if low == "w17" || low.starts_with("w17_") {
        return Err(format!(
            "eventbus: invalid channel name: {name:?} is reserved for the platform"
        ));
    }
    Ok(())
}

pub fn validate_topic(topic: &str) -> Result<(), String> {
    if topic.is_empty() {
        return Err("eventbus: invalid topic: empty".into());
    }
    for tok in topic.split('.') {
        if tok.is_empty() {
            return Err(format!(
                "eventbus: invalid topic: {topic:?} has an empty segment"
            ));
        }
        if let Some(c) = tok
            .chars()
            .find(|c| *c == '*' || *c == '>' || c.is_whitespace() || c.is_control())
        {
            return Err(format!(
                "eventbus: invalid topic: {topic:?} contains {:?}",
                c.to_string()
            ));
        }
    }
    Ok(())
}

// --- context data (eventbus/eventctx.go) --------------------------------------------

/// Incoming first, outgoing second, never a merge (eventbus.contextMD).
fn context_md(ctx: &Ctx) -> Option<&Metadata> {
    if !ctx.incoming().is_empty() {
        Some(ctx.incoming())
    } else if !ctx.outgoing().is_empty() {
        Some(ctx.outgoing())
    } else {
        None
    }
}

/// eventbus.MetadataFromContext: the request's metadata flattened for
/// `Event.metadata` — gateway-owned keys keep their FIRST value, every other
/// key its LAST; transaction keys and credentials are dropped.
pub fn metadata_from_context(ctx: &Ctx) -> Option<HashMap<String, String>> {
    let md = context_md(ctx)?;
    let mut flat = HashMap::new();
    for (k, vs) in md.iter() {
        if vs.is_empty() || reserved_metadata_key(k) {
            continue;
        }
        let v = if is_gateway_owned_key(k) {
            &vs[0]
        } else {
            &vs[vs.len() - 1]
        };
        flat.insert(k.to_string(), v.clone());
    }
    Some(flat)
}

/// eventbus.LabelsFromContext: `w17-label-*` with the prefix stripped,
/// first value; `None` when the event is unlabelled.
pub fn labels_from_context(ctx: &Ctx) -> Option<HashMap<String, String>> {
    let md = context_md(ctx)?;
    let mut labels: Option<HashMap<String, String>> = None;
    for (k, vs) in md.iter() {
        if vs.is_empty() || k.len() <= LABEL_KEY_PREFIX.len() || !k.starts_with(LABEL_KEY_PREFIX) {
            continue;
        }
        labels
            .get_or_insert_with(HashMap::new)
            .insert(k[LABEL_KEY_PREFIX.len()..].to_string(), vs[0].clone());
    }
    labels
}

// --- the envelope -----------------------------------------------------------------

/// Builds one emit's envelope from (request, response), as the generated Go
/// `emit<Svc><Method>` helper does.
///
/// A payload binding reads a field path through the request or the response
/// (Go's chained getters: an unset message on the way reads as its zero
/// value) or takes a literal string. A proto3 `optional` target gets
/// presence even for a zero value; a message target is copied only when the
/// source message is set.
pub(crate) struct EnvelopeBuilder {
    emit: Emit,
    envelope: MessageDescriptor,
    event: MessageDescriptor,
    payload: FieldDescriptor,
    metadata: FieldDescriptor,
    labels: FieldDescriptor,
    pub(crate) input: MessageDescriptor,
    pub(crate) output: MessageDescriptor,
}

fn string_map(m: HashMap<String, String>) -> Value {
    Value::Map(
        m.into_iter()
            .map(|(k, v)| (MapKey::String(k), Value::String(v)))
            .collect(),
    )
}

impl EnvelopeBuilder {
    pub(crate) fn new(
        pool: &DescriptorPool,
        emit: &Emit,
        input: MessageDescriptor,
        output: MessageDescriptor,
    ) -> Result<Self, String> {
        let envelope = pool
            .get_message_by_name(&emit.envelope)
            .ok_or_else(|| format!("no message {}", emit.envelope))?;
        let event = pool
            .get_message_by_name(&emit.event)
            .ok_or_else(|| format!("no message {}", emit.event))?;
        let payload = envelope
            .get_field_by_name(&emit.envelope_field)
            .ok_or_else(|| {
                format!(
                    "emit {}: envelope {} has no payload case {:?}",
                    emit.method, emit.envelope, emit.envelope_field
                )
            })?;
        let map_field = |name: &str| {
            envelope
                .get_field_by_name(name)
                .filter(FieldDescriptor::is_map)
                .ok_or_else(|| {
                    format!(
                        "emit {}: envelope {} has no {name} map",
                        emit.method, emit.envelope
                    )
                })
        };
        let metadata = map_field("metadata")?;
        let labels = map_field("labels")?;
        for b in &emit.fields {
            if event.get_field_by_name(&b.field).is_none() {
                return Err(format!(
                    "emit {}: event {} has no field {:?}",
                    emit.method, emit.event, b.field
                ));
            }
        }
        Ok(Self {
            emit: emit.clone(),
            envelope,
            event,
            payload,
            metadata,
            labels,
            input,
            output,
        })
    }

    pub(crate) fn emit(&self) -> &Emit {
        &self.emit
    }

    /// The marshalled envelope.
    pub(crate) fn build(&self, ctx: &Ctx, req: &[u8], resp: &[u8]) -> Result<Vec<u8>, String> {
        let req = DynamicMessage::decode(self.input.clone(), req)
            .map_err(|e| format!("decode the request: {e}"))?;
        let resp = DynamicMessage::decode(self.output.clone(), resp)
            .map_err(|e| format!("decode the response: {e}"))?;
        let mut ev = DynamicMessage::new(self.event.clone());
        for b in &self.emit.fields {
            let target = self
                .event
                .get_field_by_name(&b.field)
                .expect("checked in new");
            if b.source == "literal" {
                if target.is_list() || target.is_map() || target.kind() != Kind::String {
                    return Err(format!(
                        "payload {}: a literal fills a string field only",
                        b.field
                    ));
                }
                ev.set_field(&target, Value::String(b.value.clone()));
                continue;
            }
            let src = if b.source == "request" { &req } else { &resp };
            let (value, present) = walk(src, &b.path)?;
            if !target.is_list()
                && !target.is_map()
                && matches!(target.kind(), Kind::Message(_))
                && !present
            {
                continue;
            }
            ev.try_set_field(&target, value)
                .map_err(|e| format!("payload {}: {e}", b.field))?;
        }
        let mut env = DynamicMessage::new(self.envelope.clone());
        if let Some(md) = metadata_from_context(ctx) {
            env.set_field(&self.metadata, string_map(md));
        }
        if let Some(labels) = labels_from_context(ctx) {
            env.set_field(&self.labels, string_map(labels));
        }
        env.set_field(&self.payload, Value::Message(ev));
        Ok(env.encode_to_vec())
    }
}

fn walk(msg: &DynamicMessage, path: &[String]) -> Result<(Value, bool), String> {
    let mut cur = Value::Message(msg.clone());
    let mut present = true;
    for seg in path {
        let Value::Message(m) = &cur else {
            return Err(format!(
                "payload path {}: {seg:?} is not inside a message",
                path.join(".")
            ));
        };
        let desc = m.descriptor();
        let f = desc.get_field_by_name(seg).ok_or_else(|| {
            format!(
                "payload path {}: {} has no field {seg:?}",
                path.join("."),
                desc.full_name()
            )
        })?;
        if !f.is_list() && !f.is_map() && matches!(f.kind(), Kind::Message(_)) && !m.has_field(&f) {
            present = false;
        }
        let next = m.get_field(&f).into_owned();
        cur = next;
    }
    Ok((cur, present))
}

/// eventbus.EmitInvalidate per entity: a bare `w17.InvalidateEvent` (not
/// wrapped in the envelope) with the request's labels and no ids.
pub(crate) fn invalidate_events(inv: &Invalidate, ctx: &Ctx) -> Vec<Vec<u8>> {
    let labels: HashMap<String, String> = labels_from_context(ctx).unwrap_or_default();
    inv.entities
        .iter()
        .map(|entity| {
            InvalidateEvent {
                group: inv.group.clone(),
                message: entity.clone(),
                ids: Vec::new(),
                labels: labels.clone().into_iter().collect(),
            }
            .encode_to_vec()
        })
        .collect()
}

// --- brokers ----------------------------------------------------------------------

/// Where events go: a broker, a router over two, or a test's memory.
pub trait Dispatcher: Send + Sync {
    /// Publish one event.
    fn dispatch<'a>(
        &'a self,
        channel: &'a str,
        topic: &'a str,
        payload: Vec<u8>,
    ) -> BoxFuture<'a, Result<(), String>>;
    /// Close the connections.
    fn close(&self) -> BoxFuture<'_, ()>;
}

/// Records dispatches in memory (tests, and a tier with no broker).
#[derive(Default)]
pub struct MemoryPublisher {
    pub published: Mutex<Vec<(String, String, Vec<u8>)>>,
}

impl Dispatcher for MemoryPublisher {
    fn dispatch<'a>(
        &'a self,
        channel: &'a str,
        topic: &'a str,
        payload: Vec<u8>,
    ) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(async move {
            validate_channel(channel)?;
            validate_topic(topic)?;
            self.published
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .push((channel.into(), topic.into(), payload));
            Ok(())
        })
    }

    fn close(&self) -> BoxFuture<'_, ()> {
        Box::pin(async {})
    }
}

/// What a NATS DSN connects with.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct NatsConnectOptions {
    /// `host:port`, one per server of the list.
    pub servers: Vec<String>,
    pub user: Option<String>,
    pub pass: Option<String>,
    pub token: Option<String>,
    pub tls: bool,
}

fn redact_nats(s: &str) -> String {
    match (s.find("://"), s.rfind('@')) {
        (Some(i), Some(at)) if at > i => format!("{}***{}", &s[..i + 3], &s[at..]),
        _ => s.to_string(),
    }
}

fn percent_decode(s: &str) -> String {
    url::form_urlencoded::parse(format!("x={}", s.replace('+', "%2B")).as_bytes())
        .next()
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default()
}

/// A NATS DSN as nats.go's `nats.Connect` reads it: a comma-separated list
/// of `nats://` / `tls://` URLs (a later one without a scheme is `nats://`),
/// port 4222 by default, TLS when any is `tls://`. A URL's `user:pass@` is
/// user and password; a bare `user@` (no colon) is a TOKEN. The first URL
/// that carries credentials supplies them.
pub fn nats_connect_options(dsn: &str) -> Result<NatsConnectOptions, String> {
    let mut out = NatsConnectOptions::default();
    let mut creds = false;
    for raw in dsn.split(',') {
        let mut s = raw.trim().to_string();
        if s.is_empty() {
            continue;
        }
        if !out.servers.is_empty() && !s.contains("://") {
            s = format!("nats://{s}");
        }
        let Some((scheme, rest)) = s.split_once("://") else {
            return Err("NATS DSN must use nats:// or tls:// scheme".into());
        };
        if scheme != "nats" && scheme != "tls" {
            return Err("NATS DSN must use nats:// or tls:// scheme".into());
        }
        if scheme == "tls" {
            out.tls = true;
        }
        let url = url::Url::parse(&s)
            .map_err(|_| format!("NATS DSN: {:?} is not a URL", redact_nats(&s)))?;
        let host = url.host_str().unwrap_or("");
        if host.is_empty() {
            return Err(format!("NATS DSN: {:?} names no host", redact_nats(&s)));
        }
        out.servers
            .push(format!("{host}:{}", url.port().unwrap_or(4222)));
        let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
        let Some(at) = authority.rfind('@') else {
            continue;
        };
        if creds {
            continue;
        }
        creds = true;
        let info = &authority[..at];
        match info.split_once(':') {
            None => out.token = Some(percent_decode(info)),
            Some((u, p)) => {
                out.user = Some(percent_decode(u));
                out.pass = Some(percent_decode(p));
            }
        }
    }
    if out.servers.is_empty() {
        return Err("NATS DSN must use nats:// or tls:// scheme".into());
    }
    Ok(out)
}

/// The NATS header carrying the emitting domain (eventbus.DomainHeader).
pub const DOMAIN_HEADER: &str = "W17-Domain";

/// The headers a NATS publish carries for `domain`: the domain stamp, or
/// none for an empty domain (the wire before the stamp existed).
pub fn nats_domain_headers(domain: &str) -> Option<async_nats::HeaderMap> {
    if domain.is_empty() {
        return None;
    }
    let mut h = async_nats::HeaderMap::new();
    h.insert(DOMAIN_HEADER, domain);
    Some(h)
}

/// The XADD eventbus.RedisBus.Dispatch writes: trimmed to `min_id`, fields
/// `topic`, `payload` and, for a non-empty domain, `domain`.
pub fn redis_xadd_cmd(
    channel: &str,
    topic: &str,
    payload: Vec<u8>,
    domain: &str,
    min_id: &str,
) -> redis::Cmd {
    let mut cmd = redis::cmd("XADD");
    cmd.arg(channel).arg("MINID").arg("~").arg(min_id).arg("*");
    cmd.arg("topic").arg(topic).arg("payload").arg(payload);
    if !domain.is_empty() {
        cmd.arg("domain").arg(domain);
    }
    cmd
}

/// Publishes to NATS JetStream as eventbus.NatsBus.Dispatch does.
pub struct NatsPublisher {
    client: async_nats::Client,
    js: async_nats::jetstream::Context,
    streams: Mutex<HashSet<String>>,
    domain: String,
}

impl NatsPublisher {
    /// Connect, retrying an unreachable broker for `connect_wait`
    /// (eventbus.DefaultConnectWait: a broker started in the same deploy).
    pub async fn connect(dsn: &str, connect_wait: Duration) -> Result<Self, String> {
        let o = nats_connect_options(dsn)?;
        let deadline = tokio::time::Instant::now() + connect_wait;
        let servers: Vec<async_nats::ServerAddr> = o
            .servers
            .iter()
            .map(|s| format!("{}://{s}", if o.tls { "tls" } else { "nats" }).parse())
            .collect::<Result<_, _>>()
            .map_err(|e| format!("NATS DSN: {e}"))?;
        loop {
            let mut opts = async_nats::ConnectOptions::new()
                .connection_timeout(Duration::from_secs(2))
                .require_tls(o.tls)
                .max_reconnects(None);
            if let (Some(u), Some(p)) = (&o.user, &o.pass) {
                opts = opts.user_and_password(u.clone(), p.clone());
            }
            if let Some(t) = &o.token {
                opts = opts.token(t.clone());
            }
            match opts.connect(servers.clone()).await {
                Ok(client) => {
                    let js = async_nats::jetstream::new(client.clone());
                    return Ok(Self {
                        client,
                        js,
                        streams: Mutex::new(HashSet::new()),
                        domain: String::new(),
                    });
                }
                Err(e) if tokio::time::Instant::now() >= deadline => {
                    return Err(format!("nats connect: {e}"));
                }
                Err(_) => tokio::time::sleep(Duration::from_millis(500)).await,
            }
        }
    }

    /// Stamp every publish with `domain` (see the module doc).
    pub fn with_domain(mut self, domain: &str) -> Self {
        self.domain = domain.to_string();
        self
    }

    async fn ensure_stream(&self, channel: &str) -> Result<(), String> {
        if self
            .streams
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .contains(channel)
        {
            return Ok(());
        }
        let cfg = async_nats::jetstream::stream::Config {
            name: channel.to_string(),
            subjects: vec![format!("{channel}.>")],
            retention: async_nats::jetstream::stream::RetentionPolicy::Limits,
            discard: async_nats::jetstream::stream::DiscardPolicy::Old,
            max_age: DEFAULT_STREAM_MAX_AGE,
            max_messages: -1,
            ..Default::default()
        };
        self.js
            .create_or_update_stream(cfg)
            .await
            .map_err(|e| format!("ensure stream {channel}: {e}"))?;
        self.streams
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(channel.to_string());
        Ok(())
    }

    async fn publish(
        &self,
        subject: &str,
        payload: Vec<u8>,
    ) -> Result<(), async_nats::jetstream::context::PublishError> {
        let ack = match nats_domain_headers(&self.domain) {
            Some(h) => {
                self.js
                    .publish_with_headers(subject.to_string(), h, payload.into())
                    .await?
            }
            None => self.js.publish(subject.to_string(), payload.into()).await?,
        };
        ack.await.map(|_| ())
    }
}

impl Dispatcher for NatsPublisher {
    fn dispatch<'a>(
        &'a self,
        channel: &'a str,
        topic: &'a str,
        payload: Vec<u8>,
    ) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(async move {
            validate_channel(channel)?;
            validate_topic(topic)?;
            if self.client.connection_state() == async_nats::connection::State::Disconnected
                && self.client.server_info().max_payload == 0
            {
                return Err("eventbus nats: connection is closed".into());
            }
            let subject = format!("{channel}.{topic}");
            self.ensure_stream(channel).await?;
            match self.publish(&subject, payload.clone()).await {
                Ok(()) => Ok(()),
                Err(e)
                    if e.kind()
                        == async_nats::jetstream::context::PublishErrorKind::StreamNotFound =>
                {
                    // The broker lost the stream (recreated without
                    // persistence): forget it, re-create it, publish once more.
                    self.streams
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .remove(channel);
                    self.ensure_stream(channel).await?;
                    self.publish(&subject, payload)
                        .await
                        .map_err(|e| format!("eventbus nats: publish {subject}: {e}"))
                }
                Err(e) => Err(format!("eventbus nats: publish {subject}: {e}")),
            }
        })
    }

    fn close(&self) -> BoxFuture<'_, ()> {
        Box::pin(async move {
            if let Err(e) = self.client.drain().await {
                observability::warn(&format!("eventbus nats close: {e}"));
            }
        })
    }
}

enum RedisConn {
    Single(redis::aio::MultiplexedConnection),
    Cluster(redis::cluster_async::ClusterConnection),
}

/// Appends to Redis Streams as eventbus.RedisBus.Dispatch does.
pub struct RedisPublisher {
    conn: RedisConn,
    domain: String,
}

impl RedisPublisher {
    /// `redis://` / `rediss://`; `cluster=true` selects a cluster client
    /// (lib/redisdsn).
    pub async fn connect(dsn: &str) -> Result<Self, String> {
        let mut u = url::Url::parse(dsn).map_err(|e| format!("redis DSN: {e}"))?;
        if u.scheme() != "redis" && u.scheme() != "rediss" {
            return Err(format!(
                "redis DSN: scheme {:?}, want redis or rediss",
                u.scheme()
            ));
        }
        let vals: Vec<String> = u
            .query_pairs()
            .filter(|(k, _)| k == "cluster")
            .map(|(_, v)| v.into_owned())
            .collect();
        let mut cluster = false;
        if !vals.is_empty() {
            let v = vals[0].to_ascii_lowercase();
            if vals.len() != 1 || !["1", "t", "true", "0", "f", "false"].contains(&v.as_str()) {
                return Err("redis DSN: cluster=<bool> given more than once or not a bool".into());
            }
            cluster = ["1", "t", "true"].contains(&v.as_str());
            if cluster && !["", "/", "/0"].contains(&u.path()) {
                return Err("redis DSN: a cluster has database 0 only".into());
            }
            let rest: Vec<(String, String)> = u
                .query_pairs()
                .filter(|(k, _)| k != "cluster")
                .map(|(k, v)| (k.into_owned(), v.into_owned()))
                .collect();
            if rest.is_empty() {
                u.set_query(None);
            } else {
                u.query_pairs_mut().clear().extend_pairs(rest);
            }
            if cluster {
                u.set_path("");
            }
        }
        let conn = if cluster {
            let c = redis::cluster::ClusterClient::new(vec![u.as_str()])
                .map_err(|e| format!("redis: {e}"))?;
            RedisConn::Cluster(
                c.get_async_connection()
                    .await
                    .map_err(|e| format!("redis connect: {e}"))?,
            )
        } else {
            let c = redis::Client::open(u.as_str()).map_err(|e| format!("redis: {e}"))?;
            RedisConn::Single(
                c.get_multiplexed_async_connection()
                    .await
                    .map_err(|e| format!("redis connect: {e}"))?,
            )
        };
        let p = Self {
            conn,
            domain: String::new(),
        };
        let ping = redis::cmd("PING");
        tokio::time::timeout(Duration::from_secs(5), p.query::<String>(ping))
            .await
            .map_err(|_| "redis ping: timed out after 5000ms".to_string())??;
        Ok(p)
    }

    /// Stamp every entry with `domain` (see the module doc).
    pub fn with_domain(mut self, domain: &str) -> Self {
        self.domain = domain.to_string();
        self
    }

    async fn query<T: redis::FromRedisValue>(&self, cmd: redis::Cmd) -> Result<T, String> {
        match &self.conn {
            RedisConn::Single(c) => cmd
                .query_async(&mut c.clone())
                .await
                .map_err(|e| e.to_string()),
            RedisConn::Cluster(c) => cmd
                .query_async(&mut c.clone())
                .await
                .map_err(|e| e.to_string()),
        }
    }
}

impl Dispatcher for RedisPublisher {
    fn dispatch<'a>(
        &'a self,
        channel: &'a str,
        topic: &'a str,
        payload: Vec<u8>,
    ) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(async move {
            validate_channel(channel)?;
            validate_topic(topic)?;
            let now = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default();
            let min = now.saturating_sub(DEFAULT_STREAM_MAX_AGE).as_millis();
            let cmd = redis_xadd_cmd(channel, topic, payload, &self.domain, &format!("{min}-0"));
            self.query::<redis::Value>(cmd)
                .await
                .map(|_| ())
                .map_err(|e| format!("eventbus redis: XADD {channel}: {e}"))
        })
    }

    fn close(&self) -> BoxFuture<'_, ()> {
        Box::pin(async {})
    }
}

/// Go's eventbus `Route`: a channel declared on Redis Streams goes to the
/// Redis bus, every other channel to the NATS bus. When the bus a channel
/// belongs to is not configured (its DSN is empty) the event is dropped —
/// never sent to the other bus, because the subscriber reads each channel
/// from its declared transport only.
pub struct Router {
    pub nats: Option<Box<dyn Dispatcher>>,
    pub redis: Option<Box<dyn Dispatcher>>,
    pub redis_channels: HashSet<String>,
}

impl Dispatcher for Router {
    fn dispatch<'a>(
        &'a self,
        channel: &'a str,
        topic: &'a str,
        payload: Vec<u8>,
    ) -> BoxFuture<'a, Result<(), String>> {
        let d = if self.redis_channels.contains(channel) {
            self.redis.as_ref()
        } else {
            self.nats.as_ref()
        };
        match d {
            Some(d) => d.dispatch(channel, topic, payload),
            None => Box::pin(async { Ok(()) }),
        }
    }

    fn close(&self) -> BoxFuture<'_, ()> {
        Box::pin(async move {
            if let Some(n) = &self.nats {
                n.close().await;
            }
            if let Some(r) = &self.redis {
                r.close().await;
            }
        })
    }
}

/// Publish fire-and-forget: a failure is reported, never returned (the RPC
/// already succeeded), and bounded by the publish timeout as Go bounds each
/// Dispatch: a broker that stopped answering must not hold the committing
/// path (nor the drain).
pub(crate) async fn fire(
    dispatcher: Option<&dyn Dispatcher>,
    channel: &str,
    topic: &str,
    payload: Vec<u8>,
    what: &str,
) {
    let Some(d) = dispatcher else { return };
    let res =
        match tokio::time::timeout(DEFAULT_PUBLISH_TIMEOUT, d.dispatch(channel, topic, payload))
            .await
        {
            Ok(r) => r,
            Err(_) => Err(format!(
                "{channel}/{topic}: timed out after {}ms",
                DEFAULT_PUBLISH_TIMEOUT.as_millis()
            )),
        };
    if let Err(e) = res {
        let for_what = if what.is_empty() {
            String::new()
        } else {
            format!(" for {what}")
        };
        observability::report_error(&format!(
            "eventbus: emit {channel}/{topic}{for_what} failed: {e}"
        ));
    }
}
