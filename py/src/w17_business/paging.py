"""Paging, forwarded by hand.

The gateway turns a paged request's cursor and page size into
``x-w17-paging-{boundaries,limit,total}`` metadata. A business method bound to a
paged endpoint receives them INCOMING; the storage query it calls reads them
incoming on ITS side. Nothing copies them across the hop automatically, and that
is deliberate (``principal.forward_to_outgoing`` leaves them alone): one query's
LIMIT and keyset would bleed onto every sibling storage call the method makes.

``forward(ctx)`` is the explicit copy. Pass what it returns to the ONE call (or
the few calls) that serve the page, and nothing else; then copy the storage
response's ``w17.Paging`` into your own response. The Go twin is
``paging.Forward`` in sdk/go/lib/paging.
"""

from __future__ import annotations

from .context import Context

__all__ = ["BOUNDARIES_MD_KEY", "LIMIT_MD_KEY", "TOTAL_MD_KEY", "MD_KEYS", "forward"]

BOUNDARIES_MD_KEY = "x-w17-paging-boundaries"
LIMIT_MD_KEY = "x-w17-paging-limit"
TOTAL_MD_KEY = "x-w17-paging-total"
MD_KEYS = (BOUNDARIES_MD_KEY, LIMIT_MD_KEY, TOTAL_MD_KEY)


def forward(ctx: Context) -> Context:
    """Copy the incoming paging keys onto the outgoing side.

    The three keys travel as ONE set, as they describe one request's page: when
    the incoming side carries any of them, the outgoing copies of all three are
    replaced — so a first-page request (a limit, no boundaries) does not go out
    with boundaries left on a reused context. With none incoming the context is
    returned unchanged. Values are copied verbatim, every value of a key."""
    incoming = {k: ctx.incoming.get_all(k) for k in MD_KEYS if ctx.incoming.get_all(k)}
    if not incoming:
        return ctx
    out = ctx.outgoing.without(lambda k: k in MD_KEYS).merged(incoming)
    return ctx.with_outgoing(out)
