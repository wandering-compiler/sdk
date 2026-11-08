"""Internal TLS, read from the same variables as sdk/go/lib/grpcclient and
lib/grpcserver.

``W17_INTERNAL_TLS``:

- ``on`` | ``true`` | ``1`` | ``yes`` — servers listen TLS, clients dial TLS;
- ``terminated`` — TLS ends at the platform: servers listen plain, clients
  still dial TLS (system roots, no client pair);
- ``off`` | ``false`` | ``0`` | ``no`` | unset — plaintext;
- anything else refuses the boot (a typo must not silently downgrade).

``W17_INTERNAL_TLS_CERT`` / ``_KEY`` are this service's leaf pair (required for
a TLS server, presented as the client pair when dialing);
``W17_INTERNAL_TLS_CA`` is the roots a client verifies against;
``W17_INTERNAL_TLS_CLIENT_CA``, when set on a server, requires and verifies a
client certificate (mutual TLS).
"""

from __future__ import annotations

import os
import ssl
from collections.abc import Mapping

__all__ = [
    "ENV_INTERNAL_TLS",
    "ENV_INTERNAL_TLS_CA",
    "ENV_INTERNAL_TLS_CERT",
    "ENV_INTERNAL_TLS_KEY",
    "ENV_INTERNAL_TLS_CLIENT_CA",
    "check",
    "enabled",
    "terminated",
    "dial_tls",
    "client_context",
    "server_context",
]

ENV_INTERNAL_TLS = "W17_INTERNAL_TLS"
ENV_INTERNAL_TLS_CA = "W17_INTERNAL_TLS_CA"
ENV_INTERNAL_TLS_CERT = "W17_INTERNAL_TLS_CERT"
ENV_INTERNAL_TLS_KEY = "W17_INTERNAL_TLS_KEY"
ENV_INTERNAL_TLS_CLIENT_CA = "W17_INTERNAL_TLS_CLIENT_CA"


def enabled(v: str) -> bool:
    return v.strip().lower() in ("on", "true", "1", "yes")


def terminated(v: str) -> bool:
    return v.strip().lower() == "terminated"


def _off(v: str) -> bool:
    return v.strip().lower() in ("", "off", "false", "0", "no")


def check(v: str) -> None:
    if enabled(v) or terminated(v) or _off(v):
        return
    raise ValueError(
        f"{ENV_INTERNAL_TLS}={v!r} is not a recognised value; accepted: on|true|1|yes (TLS), "
        "terminated (TLS ends at the platform; servers listen plain), off|false|0|no or unset (plaintext)"
    )


def dial_tls(v: str) -> bool:
    return enabled(v) or terminated(v)


def client_context(env: Mapping[str, str] | None = None) -> ssl.SSLContext | None:
    """The SSL context a client dials with, or None for plaintext.

    An unrecognised value dials TLS (Go's grpcx.InternalTLSEnabled logs and
    dials TLS: refusing a call is safer than sending it in the clear)."""
    env = os.environ if env is None else env
    v = env.get(ENV_INTERNAL_TLS, "")
    try:
        check(v)
    except ValueError:
        pass
    else:
        if not dial_tls(v):
            return None
    ctx = ssl.create_default_context(ssl.Purpose.SERVER_AUTH)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.set_alpn_protocols(["h2"])
    if terminated(v):
        return ctx
    ca = env.get(ENV_INTERNAL_TLS_CA, "")
    if ca and os.path.exists(ca):
        ctx.load_verify_locations(cafile=ca)
    cert, key = env.get(ENV_INTERNAL_TLS_CERT, ""), env.get(ENV_INTERNAL_TLS_KEY, "")
    if cert and key:
        try:
            ctx.load_cert_chain(cert, key)
        except (OSError, ssl.SSLError):
            # Go skips a pair it cannot load and dials without one; the
            # server then refuses the handshake if it requires one.
            pass
    return ctx


def server_context(env: Mapping[str, str] | None = None) -> ssl.SSLContext | None:
    """The SSL context the server listens with, or None for plaintext.

    Raises ValueError for a value that is not recognised, or TLS on without a
    loadable leaf pair — the boot fails naming the variable."""
    env = os.environ if env is None else env
    v = env.get(ENV_INTERNAL_TLS, "")
    check(v)
    if not enabled(v):
        return None
    cert, key = env.get(ENV_INTERNAL_TLS_CERT, ""), env.get(ENV_INTERNAL_TLS_KEY, "")
    if not cert or not key:
        raise ValueError(
            f"{ENV_INTERNAL_TLS}=on but {ENV_INTERNAL_TLS_CERT} / {ENV_INTERNAL_TLS_KEY} are not both set — "
            "point them at this service's leaf cert+key (dev PKI: w17/certs/dev.local/)"
        )
    ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    try:
        ctx.load_cert_chain(cert, key)
    except (OSError, ssl.SSLError) as e:
        raise ValueError(f"loading server cert/key ({ENV_INTERNAL_TLS_CERT}/{ENV_INTERNAL_TLS_KEY}): {e}") from e
    client_ca = env.get(ENV_INTERNAL_TLS_CLIENT_CA, "")
    if client_ca:
        try:
            ctx.load_verify_locations(cafile=client_ca)
        except (OSError, ssl.SSLError) as e:
            raise ValueError(f"reading {ENV_INTERNAL_TLS_CLIENT_CA} at {client_ca!r}: {e}") from e
        ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.set_alpn_protocols(["h2"])
    return ctx


def otlp_credentials(env: Mapping[str, str]):
    """grpcio channel credentials for the OTLP exporter, from the mesh's CA and
    client pair (system roots when no CA is set)."""
    import grpc

    def read(name: str) -> bytes | None:
        p = env.get(name, "")
        if p and os.path.exists(p):
            with open(p, "rb") as f:
                return f.read()
        return None

    if terminated(env.get(ENV_INTERNAL_TLS, "")):
        return grpc.ssl_channel_credentials()
    return grpc.ssl_channel_credentials(
        root_certificates=read(ENV_INTERNAL_TLS_CA),
        private_key=read(ENV_INTERNAL_TLS_KEY),
        certificate_chain=read(ENV_INTERNAL_TLS_CERT),
    )
