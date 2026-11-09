"""The verified caller, read from a handler's context, and relayed onward.

The Python twin of ``sdk/go/lib/principal``: the same metadata keys, the same
fail-closed readers, the same relay set and replace semantics. Any change there
is a change here (and a conformance case).

The gateway stamps the principal as gRPC metadata on its first hop:

- ``x-w17-user`` — base64 of the marshalled ``AuthResp`` (always present when
  authenticated);
- ``x-w17-scope-<name>`` — one per declared data scope;
- ``w17-label-<name>`` — broadcast labels;
- ``x-w17-device``, ``x-w17-dpop-jkt``, ``x-w17-data-scope``,
  ``x-w17-audience``, ``x-w17-membership-kinds``, ``x-w17-org-from``,
  ``x-w17-host``.

Trust boundary: the envelope is NOT signed. A tier believes it only because the
gateway strips these namespaces from client input and backend tiers are reachable
only from trusted tiers. Keep the business container off untrusted networks.

``AuthResp`` is generated per project, so this module cannot name the type: the
reader takes the message class, or uses the one the SDK resolved at boot
(``set_auth_resp_type``; the manifest names it, otherwise the descriptor set's
single ``AuthResp``).
"""

from __future__ import annotations

import base64
import binascii
from collections.abc import Mapping

from google.protobuf.message import DecodeError, Message

from .context import Context, Metadata

__all__ = [
    "ErrNoPrincipal",
    "NoPrincipalError",
    "USER_METADATA_KEY",
    "SCOPE_KEY_PREFIX",
    "LABEL_KEY_PREFIX",
    "DEVICE_METADATA_KEY",
    "MINT_KEY_METADATA_KEY",
    "DATA_SCOPE_METADATA_KEY",
    "AUDIENCE_KEY",
    "MEMBERSHIP_KINDS_KEY",
    "ORG_FROM_KEY",
    "HOST_KEY",
    "DATA_SCOPE_OWNER",
    "DATA_SCOPE_ORG",
    "DATA_SCOPE_ALL",
    "user_id",
    "envelope",
    "scope",
    "label",
    "device_id",
    "data_scope",
    "scope_key",
    "label_key",
    "forward_to_outgoing",
    "act_as",
    "is_relayed_key",
    "is_gateway_owned_key",
    "set_auth_resp_type",
]

USER_METADATA_KEY = "x-w17-user"
SCOPE_KEY_PREFIX = "x-w17-scope-"
LABEL_KEY_PREFIX = "w17-label-"
DEVICE_METADATA_KEY = "x-w17-device"
MINT_KEY_METADATA_KEY = "x-w17-dpop-jkt"
DATA_SCOPE_METADATA_KEY = "x-w17-data-scope"
AUDIENCE_KEY = "x-w17-audience"
MEMBERSHIP_KINDS_KEY = "x-w17-membership-kinds"
ORG_FROM_KEY = "x-w17-org-from"
HOST_KEY = "x-w17-host"

DATA_SCOPE_OWNER = "owner"
DATA_SCOPE_ORG = "org"
DATA_SCOPE_ALL = "all"

# The exact relayed keys beside the two prefixes — principal.isRelayedKey.
_RELAYED_EXACT = frozenset(
    {
        USER_METADATA_KEY,
        DATA_SCOPE_METADATA_KEY,
        DEVICE_METADATA_KEY,
        MINT_KEY_METADATA_KEY,
        AUDIENCE_KEY,
        MEMBERSHIP_KINDS_KEY,
        ORG_FROM_KEY,
        HOST_KEY,
    }
)

# principal.gatewayOwnedPrefixes / gatewayOwnedKeys.
_GATEWAY_OWNED_PREFIXES = ("x-w17-", LABEL_KEY_PREFIX)
_GATEWAY_OWNED_KEYS = ("w17-tx-id", "w17-conn-id")


class NoPrincipalError(Exception):
    """No verified caller could be read from the context.

    Every failure mode collapses to this — absent metadata, an undecodable
    envelope, an empty user id — because the distinction is never actionable
    for the caller and leaking it tells an unauthenticated prober how far it
    got. The cause stays chained (``__cause__``) for a server-side log."""

    def __init__(self, detail: str = "") -> None:
        msg = "principal: no authenticated caller in context"
        super().__init__(f"{msg}: {detail}" if detail else msg)


# Go's sentinel name, for code that reads like the Go guide.
ErrNoPrincipal = NoPrincipalError

_auth_resp_type: type[Message] | None = None


def set_auth_resp_type(cls: type[Message] | None) -> None:
    """The project's AuthResp message class the readers decode into by default.
    The runtime sets it at boot; tests may set it."""
    global _auth_resp_type
    _auth_resp_type = cls


def _resolve(dst: Message | type[Message] | None) -> Message:
    if dst is None:
        if _auth_resp_type is None:
            raise NoPrincipalError("no AuthResp type is known (pass the message class)")
        return _auth_resp_type()
    if isinstance(dst, type):
        return dst()
    return dst


def scope_key(name: str) -> str:
    """``org_id`` → ``x-w17-scope-org_id``."""
    return SCOPE_KEY_PREFIX + name


def label_key(name: str) -> str:
    """``account_kind`` → ``w17-label-account_kind``."""
    return LABEL_KEY_PREFIX + name


def user_id(ctx: Context, dst: Message | type[Message] | None = None) -> str:
    """The authenticated caller's user id; raises NoPrincipalError.

    Reads the cheap ``x-w17-scope-user_id`` first, then decodes the envelope —
    the fallback is what makes it work in a project scoped by something other
    than user_id, where the gateway threads no user_id scope."""
    v = ctx.incoming.first(scope_key("user_id"))
    if v:
        return v
    msg = _envelope_from_md(ctx.incoming, _resolve(dst))
    uid = getattr(msg, "user_id", "")
    if uid:
        return uid
    raise NoPrincipalError()


def envelope(ctx: Context, dst: Message | type[Message] | None = None) -> Message:
    """Decode the full verified principal (the project's AuthResp).

    Returns the decoded message (filled in place when dst is an instance)."""
    return _envelope_from_md(ctx.incoming, _resolve(dst))


def _envelope_from_md(md: Metadata, dst: Message) -> Message:
    v = md.first(USER_METADATA_KEY)
    if not v:
        raise NoPrincipalError()
    try:
        raw = base64.b64decode(v, validate=True)
    except (binascii.Error, ValueError) as e:
        raise NoPrincipalError("undecodable envelope") from e
    try:
        dst.ParseFromString(raw)
    except DecodeError as e:
        raise NoPrincipalError("unmarshal envelope") from e
    return dst


def scope(ctx: Context, name: str) -> str | None:
    """A declared data scope's value, None when absent. Fail closed on None —
    an absent scope is never "match everything"."""
    v = ctx.incoming.first(scope_key(name))
    return v or None


def label(ctx: Context, name: str) -> str | None:
    """A broadcast label, None when absent. Absence must deny: a rule reads
    "refuse unless the label says X"."""
    v = ctx.incoming.first(label_key(name))
    return v or None


def device_id(ctx: Context) -> str | None:
    """The verified device id (DPoP key thumbprint), None when absent. Exactly
    one non-empty value counts, as in Go."""
    vals = ctx.incoming.get_all(DEVICE_METADATA_KEY)
    if len(vals) != 1 or not vals[0]:
        return None
    return vals[0]


def data_scope(ctx: Context) -> str:
    """The audience data range: "owner", "org", "all", or "" when none.

    An unknown value, or several, reads as the narrowest range ("owner")."""
    vals = ctx.incoming.get_all(DATA_SCOPE_METADATA_KEY)
    if not vals:
        return ""
    if len(vals) == 1 and vals[0] in (DATA_SCOPE_OWNER, DATA_SCOPE_ORG, DATA_SCOPE_ALL):
        return vals[0]
    return DATA_SCOPE_OWNER


def is_relayed_key(key: str) -> bool:
    """Whether a (lower-case) key is one ``forward_to_outgoing`` relays."""
    return key in _RELAYED_EXACT or key.startswith(SCOPE_KEY_PREFIX) or key.startswith(LABEL_KEY_PREFIX)


def is_gateway_owned_key(key: str) -> bool:
    """principal.IsGatewayOwnedKey: ``x-w17-*``, ``w17-label-*``, and the exact
    ``w17-tx-id`` / ``w17-conn-id``. Case-insensitive."""
    k = key.strip().lower()
    return k.startswith(_GATEWAY_OWNED_PREFIXES) or k in _GATEWAY_OWNED_KEYS


def forward_to_outgoing(ctx: Context) -> Context:
    """Relay the verified principal from INCOMING onto OUTGOING metadata.

    Moves exactly the gateway stamps (see ``is_relayed_key``) and nothing else:
    paging, tracing, locale and transaction routing have their own propagation.

    The verified principal wins: when the incoming side carries any relayed
    key, the outgoing side's whole relayed set is REPLACED — an outgoing value
    for a relayed key is overwritten, and an outgoing-only relayed key is
    dropped, because nothing distinguishes a deliberate override from a context
    reused across requests, and the second would scope the next hop to someone
    else's rows. Every other outgoing key is kept. With no incoming principal
    the context is returned unchanged. Idempotent.

    Every storage client call applies it, as Go's ``grpcx.DialOpts`` does."""
    relayed = {k: v for k, v in ctx.incoming.items() if is_relayed_key(k) and v}
    if not relayed:
        return ctx
    out = ctx.outgoing.without(is_relayed_key).merged(relayed)
    return ctx.with_outgoing(out)


def act_as(ctx: Context, user: Message, scopes: Mapping[str, str] | None = None) -> Context:
    """A context that calls a tier of THIS domain as ``user``.

    The envelope (the project's AuthResp; its user id is required) and the named
    scopes are stamped on the OUTGOING side in place of any principal ctx
    carries, and every relayed key is dropped from the INCOMING side too, so a
    later ``forward_to_outgoing`` cannot put a different principal back.

    Raises ValueError for a missing user id or a malformed scope."""
    if user is None or not getattr(user, "user_id", ""):
        raise ValueError("principal: act_as needs a user id")
    stamps: dict[str, tuple[str, ...]] = {
        USER_METADATA_KEY: (base64.b64encode(user.SerializeToString()).decode("ascii"),)
    }
    for name, v in (scopes or {}).items():
        if not name or not v or name.strip() != name:
            raise ValueError(f"principal: act_as: scope {name!r}={v!r} is not a scope")
        stamps[scope_key(name)] = (v,)
    ctx = ctx.with_incoming(ctx.incoming.without(is_relayed_key))
    return ctx.with_outgoing(ctx.outgoing.without(is_relayed_key).merged(stamps))
