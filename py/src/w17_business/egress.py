"""Egress clients: the project's third-party REST APIs, served by the business companion.

A Go business tier calls an egress client (``proto/clients/<name>/``) through
``sdk/go/lib/egress`` in-process; this tier calls the same Go code in the
companion, through the client's own gRPC service, unchanged::

    quote = await clients.egress("quotes").GetQuote(ctx, req)

The companion holds the client's ``W17_CLIENT_<NAME>_DSN``, retries what is
safe to retry (429/502/503/504 and transport errors, idempotent methods only)
and caps a call at 30 s; the SDK sends one attempt. A failure is a
``GRPCError`` whose status describes the caller's situation (404 →
``NOT_FOUND``, 409 → ``ABORTED``, 429/5xx → ``UNAVAILABLE``, …) and which
carries a ``google.rpc.ErrorInfo`` — ``error_info(err)`` reads it::

    except GRPCError as e:
        info = egress.error_info(e)  # ErrorInfo(domain="quotes", reason="UPSTREAM_HTTP_404", metadata={...})

Re-raising the error answers with it unchanged, as a Go handler returning
``err`` does; the gateway does not show its upstream text to an end user.

Egress hooks (Go's ``BeforeRequest`` / ``AfterResponse``) are Go functions: a
client that needs one stays on a Go tier.
"""

from __future__ import annotations

from .errors import ErrorInfo, error_info

__all__ = ["ErrorInfo", "error_info"]
