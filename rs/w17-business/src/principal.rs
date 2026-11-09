//! The verified caller, read from a handler's context, and relayed onward.
//!
//! The Rust twin of `sdk/go/lib/principal`: the same metadata keys, the same
//! fail-closed readers, the same relay set and replace semantics. Any change
//! there is a change here (and a conformance case).
//!
//! The gateway stamps the principal as gRPC metadata on its first hop:
//! `x-w17-user` (base64 of the marshalled `AuthResp`), `x-w17-scope-<name>`
//! per declared data scope, `w17-label-<name>` broadcast labels, and
//! `x-w17-device`, `x-w17-dpop-jkt`, `x-w17-data-scope`, `x-w17-audience`,
//! `x-w17-membership-kinds`, `x-w17-org-from`, `x-w17-host`.
//!
//! Trust boundary: the envelope is NOT signed. A tier believes it only
//! because the gateway strips these namespaces from client input and backend
//! tiers are reachable only from trusted tiers. Keep the business container
//! off untrusted networks.
//!
//! `AuthResp` is generated per project: [`envelope`] decodes into the type
//! you name, and the readers that need only its user id decode it through
//! the descriptor set's single `AuthResp`, which the runtime resolves at boot.

use std::{fmt, sync::RwLock};

use base64::Engine as _;
use prost_reflect::{DynamicMessage, MessageDescriptor};
use tonic::Status;

use crate::{ctx::Ctx, metadata::Metadata};

pub const USER_METADATA_KEY: &str = "x-w17-user";
pub const SCOPE_KEY_PREFIX: &str = "x-w17-scope-";
pub const LABEL_KEY_PREFIX: &str = "w17-label-";
pub const DEVICE_METADATA_KEY: &str = "x-w17-device";
pub const MINT_KEY_METADATA_KEY: &str = "x-w17-dpop-jkt";
pub const DATA_SCOPE_METADATA_KEY: &str = "x-w17-data-scope";
pub const AUDIENCE_KEY: &str = "x-w17-audience";
pub const MEMBERSHIP_KINDS_KEY: &str = "x-w17-membership-kinds";
pub const ORG_FROM_KEY: &str = "x-w17-org-from";
pub const HOST_KEY: &str = "x-w17-host";

pub const DATA_SCOPE_OWNER: &str = "owner";
pub const DATA_SCOPE_ORG: &str = "org";
pub const DATA_SCOPE_ALL: &str = "all";

/// The exact relayed keys beside the two prefixes (principal.isRelayedKey).
const RELAYED_EXACT: &[&str] = &[
    USER_METADATA_KEY,
    DATA_SCOPE_METADATA_KEY,
    DEVICE_METADATA_KEY,
    MINT_KEY_METADATA_KEY,
    AUDIENCE_KEY,
    MEMBERSHIP_KINDS_KEY,
    ORG_FROM_KEY,
    HOST_KEY,
];

/// principal.gatewayOwnedPrefixes / gatewayOwnedKeys.
const GATEWAY_OWNED_PREFIXES: &[&str] = &["x-w17-", LABEL_KEY_PREFIX];
const GATEWAY_OWNED_KEYS: &[&str] = &["w17-tx-id", "w17-conn-id"];

/// No verified caller could be read from the context.
///
/// Every failure mode collapses to this — absent metadata, an undecodable
/// envelope, an empty user id — because the distinction is never actionable
/// for the caller and leaking it tells an unauthenticated prober how far it
/// got. As a status it is `UNKNOWN` with its message, as Go returns it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NoPrincipal {
    detail: String,
}

impl NoPrincipal {
    fn new(detail: impl Into<String>) -> Self {
        Self {
            detail: detail.into(),
        }
    }
}

impl fmt::Display for NoPrincipal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("principal: no authenticated caller in context")?;
        if !self.detail.is_empty() {
            write!(f, ": {}", self.detail)?;
        }
        Ok(())
    }
}

impl std::error::Error for NoPrincipal {}

impl From<NoPrincipal> for Status {
    fn from(e: NoPrincipal) -> Self {
        Status::unknown(e.to_string())
    }
}

static AUTH_RESP: RwLock<Option<MessageDescriptor>> = RwLock::new(None);

/// The project's `AuthResp`, which the readers decode the envelope through.
/// The runtime sets it at boot (the descriptor set's one message named
/// `AuthResp`); tests may set it.
pub fn set_auth_resp_type(desc: Option<MessageDescriptor>) {
    *AUTH_RESP.write().unwrap_or_else(|e| e.into_inner()) = desc;
}

fn auth_resp_type() -> Option<MessageDescriptor> {
    AUTH_RESP.read().unwrap_or_else(|e| e.into_inner()).clone()
}

/// `org_id` → `x-w17-scope-org_id`.
pub fn scope_key(name: &str) -> String {
    format!("{SCOPE_KEY_PREFIX}{name}")
}

/// `account_kind` → `w17-label-account_kind`.
pub fn label_key(name: &str) -> String {
    format!("{LABEL_KEY_PREFIX}{name}")
}

fn envelope_bytes(md: &Metadata) -> Result<Vec<u8>, NoPrincipal> {
    let v = md
        .first(USER_METADATA_KEY)
        .filter(|v| !v.is_empty())
        .ok_or_else(|| NoPrincipal::new(""))?;
    // Go's base64.StdEncoding is strict: padding required, no whitespace.
    base64::engine::general_purpose::STANDARD
        .decode(v)
        .map_err(|_| NoPrincipal::new("undecodable envelope"))
}

fn dynamic_envelope(md: &Metadata) -> Result<DynamicMessage, NoPrincipal> {
    let desc = auth_resp_type().ok_or_else(|| NoPrincipal::new("no AuthResp type is known"))?;
    let raw = envelope_bytes(md)?;
    DynamicMessage::decode(desc, raw.as_slice()).map_err(|_| NoPrincipal::new("unmarshal envelope"))
}

fn user_id_of(msg: &DynamicMessage) -> Option<String> {
    msg.get_field_by_name("user_id")
        .and_then(|v| v.as_str().map(str::to_string))
        .filter(|s| !s.is_empty())
}

/// The authenticated caller's user id; fails closed with [`NoPrincipal`].
///
/// Reads the cheap `x-w17-scope-user_id` first, then decodes the envelope —
/// the fallback is what makes it work in a project scoped by something other
/// than user_id, where the gateway threads no user_id scope.
pub fn user_id(ctx: &Ctx) -> Result<String, NoPrincipal> {
    if let Some(v) = ctx
        .incoming()
        .first(&scope_key("user_id"))
        .filter(|v| !v.is_empty())
    {
        return Ok(v.to_string());
    }
    let msg = dynamic_envelope(ctx.incoming())?;
    user_id_of(&msg).ok_or_else(|| NoPrincipal::new(""))
}

/// The full verified principal, decoded into the project's `AuthResp` type
/// (`principal::envelope::<pb::shop::AuthResp>(&ctx)`).
pub fn envelope<T: prost::Message + Default>(ctx: &Ctx) -> Result<T, NoPrincipal> {
    let raw = envelope_bytes(ctx.incoming())?;
    T::decode(raw.as_slice()).map_err(|_| NoPrincipal::new("unmarshal envelope"))
}

/// A declared data scope's value, `None` when absent. Fail closed on `None`
/// — an absent scope is never "match everything".
pub fn scope(ctx: &Ctx, name: &str) -> Option<String> {
    ctx.incoming()
        .first(&scope_key(name))
        .filter(|v| !v.is_empty())
        .map(str::to_string)
}

/// A broadcast label, `None` when absent. Absence must deny: a rule reads
/// "refuse unless the label says X".
pub fn label(ctx: &Ctx, name: &str) -> Option<String> {
    ctx.incoming()
        .first(&label_key(name))
        .filter(|v| !v.is_empty())
        .map(str::to_string)
}

/// The verified device id (DPoP key thumbprint), `None` when absent. Exactly
/// one non-empty value counts, as in Go.
pub fn device_id(ctx: &Ctx) -> Option<String> {
    match ctx.incoming().get_all(DEVICE_METADATA_KEY) {
        [v] if !v.is_empty() => Some(v.clone()),
        _ => None,
    }
}

/// The audience data range: "owner", "org", "all", or "" when none. An
/// unknown value, or several, reads as the narrowest range ("owner").
pub fn data_scope(ctx: &Ctx) -> String {
    match ctx.incoming().get_all(DATA_SCOPE_METADATA_KEY) {
        [] => String::new(),
        [v] if [DATA_SCOPE_OWNER, DATA_SCOPE_ORG, DATA_SCOPE_ALL].contains(&v.as_str()) => {
            v.clone()
        }
        _ => DATA_SCOPE_OWNER.to_string(),
    }
}

/// Whether a (lower-case) key is one [`forward_to_outgoing`] relays.
pub fn is_relayed_key(key: &str) -> bool {
    RELAYED_EXACT.contains(&key)
        || key.starts_with(SCOPE_KEY_PREFIX)
        || key.starts_with(LABEL_KEY_PREFIX)
}

/// principal.IsGatewayOwnedKey: `x-w17-*`, `w17-label-*`, and the exact
/// `w17-tx-id` / `w17-conn-id`. Case-insensitive.
pub fn is_gateway_owned_key(key: &str) -> bool {
    let k = key.trim().to_ascii_lowercase();
    GATEWAY_OWNED_PREFIXES.iter().any(|p| k.starts_with(p))
        || GATEWAY_OWNED_KEYS.contains(&k.as_str())
}

/// Relay the verified principal from INCOMING onto OUTGOING metadata.
///
/// Moves exactly the gateway stamps (see [`is_relayed_key`]) and nothing
/// else: paging, tracing, locale and transaction routing have their own
/// propagation. The verified principal wins: when the incoming side carries
/// any relayed key, the outgoing side's whole relayed set is REPLACED — an
/// outgoing value for a relayed key is overwritten, an outgoing-only relayed
/// key dropped — because nothing distinguishes a deliberate override from a
/// context reused across requests. Every other outgoing key is kept. With no
/// incoming principal the context is returned unchanged. Every call through
/// the SDK's channels applies it, as Go's `grpcx.DialOpts` does.
pub fn forward_to_outgoing(ctx: &Ctx) -> Ctx {
    let relayed: Metadata = ctx
        .incoming()
        .iter()
        .filter(|(k, v)| is_relayed_key(k) && !v.is_empty())
        .flat_map(|(k, vs)| vs.iter().map(move |v| (k, v.clone())))
        .collect();
    if relayed.is_empty() {
        return ctx.clone();
    }
    ctx.with_outgoing(ctx.outgoing().without(is_relayed_key).merged(&relayed))
}

/// Why [`act_as`] refused.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ActAsError(String);

impl fmt::Display for ActAsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ActAsError {}

impl From<ActAsError> for Status {
    fn from(e: ActAsError) -> Self {
        Status::internal(e.0)
    }
}

/// A context that calls a tier of THIS domain as `user` (the project's
/// `AuthResp`; its user id is required).
///
/// The envelope and the named scopes are stamped on the OUTGOING side in
/// place of any principal `ctx` carries, and every relayed key is dropped
/// from the INCOMING side too, so a later [`forward_to_outgoing`] cannot put
/// a different principal back.
pub fn act_as<M: prost::Message>(
    ctx: &Ctx,
    user: &M,
    scopes: &[(&str, &str)],
) -> Result<Ctx, ActAsError> {
    let raw = user.encode_to_vec();
    let desc = auth_resp_type()
        .ok_or_else(|| ActAsError("principal: act_as: no AuthResp type is known".into()))?;
    let msg = DynamicMessage::decode(desc, raw.as_slice()).map_err(|e| {
        ActAsError(format!(
            "principal: act_as: the user is not an AuthResp: {e}"
        ))
    })?;
    if user_id_of(&msg).is_none() {
        return Err(ActAsError("principal: act_as needs a user id".into()));
    }
    let mut stamps = vec![(
        USER_METADATA_KEY.to_string(),
        base64::engine::general_purpose::STANDARD.encode(&raw),
    )];
    for (name, v) in scopes {
        if name.is_empty() || v.is_empty() || name.trim() != *name {
            return Err(ActAsError(format!(
                "principal: act_as: scope {name:?}={v:?} is not a scope"
            )));
        }
        stamps.push((scope_key(name), v.to_string()));
    }
    let stamps = Metadata::from_pairs(stamps);
    let stripped = ctx.with_incoming(ctx.incoming().without(is_relayed_key));
    Ok(stripped.with_outgoing(stripped.outgoing().without(is_relayed_key).merged(&stamps)))
}
