"""``python -m w17_business serve|health [--manifest PATH]``.

- ``serve`` (also the default with no command) boots and serves the tier — what
  the container image runs.
- ``health [--timeout DURATION]`` asks the listener this tier opens whether it
  answers ``grpc.health.v1`` SERVING, on this host — the container HEALTHCHECK,
  as Go's ``<binary> health``. Exit 0 when it answered, 1 naming it otherwise.
  The address comes from the same environment the server reads.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import re
import ssl
import sys

from . import tlsconfig
from .manifest import ManifestError, load
from .runtime import BootError, manifest_path, serve

DEFAULT_HEALTH_TIMEOUT = 3.0

_DUR = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m)$")


def parse_duration(s: str) -> float:
    """Go's duration syntax for the cases a probe takes: ``500ms``, ``3s``, ``1m``."""
    m = _DUR.match(s.strip())
    if not m:
        raise ValueError(f"invalid duration {s!r}")
    n, unit = float(m.group(1)), m.group(2)
    return n / 1000 if unit == "ms" else n * 60 if unit == "m" else n


def _probe_target(listen: str) -> tuple[str, int]:
    host, sep, port = listen.rpartition(":")
    if not sep or not port.isdigit() or port == "0":
        raise ValueError(f"listen address {listen!r} has no fixed port to probe")
    host = host.strip("[]")
    if host in ("", "0.0.0.0", "::"):
        host = "127.0.0.1"
    return host, int(port)


def _der(buf: bytes, i: int) -> tuple[int, int, int]:
    """One DER TLV at i: (tag, start of the value, end of the value)."""
    tag, n = buf[i], buf[i + 1]
    i += 2
    if n & 0x80:
        size = n & 0x7F
        n = int.from_bytes(buf[i:i + size], "big")
        i += size
    if i + n > len(buf):
        raise ValueError("truncated DER")
    return tag, i, i + n


def _children(buf: bytes, start: int, end: int):
    while start < end:
        tag, vs, ve = _der(buf, start)
        yield tag, vs, ve
        start = ve


_SAN_OID = bytes([0x06, 0x03, 0x55, 0x1D, 0x11])  # 2.5.29.17 subjectAltName


def _first_dns_name(der: bytes) -> str | None:
    """The first dNSName of a certificate's subjectAltName, read with nothing
    beyond the DER the stdlib hands out (ssl.PEM_cert_to_DER_cert)."""
    _, cs, ce = _der(der, 0)                     # Certificate
    _, ts, te = next(_children(der, cs, ce))     # tbsCertificate
    for tag, vs, ve in _children(der, ts, te):
        if tag != 0xA3:                          # [3] extensions
            continue
        _, es, ee = _der(der, vs)                # SEQUENCE OF Extension
        for _, xs, xe in _children(der, es, ee):
            parts = list(_children(der, xs, xe))
            if der[parts[0][1] - 2:parts[0][2]] != _SAN_OID:
                continue
            _, os_, _ = parts[-1]                # extnValue OCTET STRING
            _, gs, ge = _der(der, os_)           # GeneralNames
            for gtag, nvs, nve in _children(der, gs, ge):
                if gtag == 0x82:                 # [2] dNSName
                    return der[nvs:nve].decode("ascii")
    return None


def _leaf_server_name(cert_path: str) -> str | None:
    # The listener's certificate names this service, not 127.0.0.1: verify the
    # probe against the container's own leaf's first DNS name, as Go does.
    try:
        with open(cert_path, encoding="ascii") as f:
            pem = f.read()
        end = pem.index(ssl.PEM_FOOTER) + len(ssl.PEM_FOOTER)
        return _first_dns_name(ssl.PEM_cert_to_DER_cert(pem[pem.index(ssl.PEM_HEADER):end]))
    except Exception:  # noqa: BLE001 — no name: the probe verifies against the host it dials
        return None


async def probe(listen: str, env: dict[str, str], timeout: float) -> None:
    from grpclib.client import Channel
    from grpclib.config import Configuration
    from grpclib.health.v1.health_grpc import HealthStub
    from grpclib.health.v1.health_pb2 import HealthCheckRequest, HealthCheckResponse

    host, port = _probe_target(listen)
    v = env.get(tlsconfig.ENV_INTERNAL_TLS, "")
    tlsconfig.check(v)
    ssl_ctx = None
    config = None
    if tlsconfig.enabled(v):
        cert, key = env.get(tlsconfig.ENV_INTERNAL_TLS_CERT, ""), env.get(tlsconfig.ENV_INTERNAL_TLS_KEY, "")
        if not cert or not key:
            raise ValueError(
                f"{tlsconfig.ENV_INTERNAL_TLS} is on but {tlsconfig.ENV_INTERNAL_TLS_CERT} / "
                f"{tlsconfig.ENV_INTERNAL_TLS_KEY} are not both set — the listener cannot be serving TLS either")
        ssl_ctx = tlsconfig.client_context({**env, tlsconfig.ENV_INTERNAL_TLS: "on"})
        name = _leaf_server_name(cert)
        if name:
            config = Configuration(ssl_target_name_override=name)
    ch = Channel(host, port, ssl=ssl_ctx, config=config)
    try:
        resp = await asyncio.wait_for(HealthStub(ch).Check(HealthCheckRequest()), timeout)
    finally:
        ch.close()
    if resp.status != HealthCheckResponse.SERVING:
        raise RuntimeError(f"grpc.health.v1 status {HealthCheckResponse.ServingStatus.Name(resp.status)}")


def health(manifest: str | None, timeout: float) -> int:
    m = load(manifest_path(manifest))
    listen = os.environ.get(m.listen_env) or m.default_listen_addr
    try:
        asyncio.run(probe(listen, dict(os.environ), timeout))
    except Exception as e:  # noqa: BLE001
        print(f"FAIL grpc {listen}: {e}")
        print("health: grpc not answering", file=sys.stderr)
        return 1
    print(f"ok   grpc {listen}")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="w17_business", description="w17 business tier (Python)")
    sub = ap.add_subparsers(dest="cmd")
    s = sub.add_parser("serve", help="boot and serve the tier (default)")
    s.add_argument("--manifest", help="path to w17-business.json (default $W17_BUSINESS_MANIFEST, else ./w17-business.json)")
    s.add_argument("--project-root", help="the directory register.path is relative to (default $W17_BUSINESS_PROJECT_ROOT, else the working directory)")
    h = sub.add_parser("health", help="probe this tier's listener (a container HEALTHCHECK)")
    h.add_argument("--manifest")
    h.add_argument("--timeout", default="3s", help="bound on the whole probe (default 3s)")
    args = ap.parse_args(argv)
    try:
        if args.cmd == "health":
            t = parse_duration(args.timeout)
            if t <= 0:
                print(f"health: --timeout must be positive, got {args.timeout}", file=sys.stderr)
                return 2
            return health(args.manifest, t)
        serve(getattr(args, "manifest", None), getattr(args, "project_root", None))
        return 0
    except (BootError, ManifestError) as e:
        print(f"w17_business: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
