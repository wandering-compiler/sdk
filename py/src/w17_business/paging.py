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

__all__ = [
    "BOUNDARIES_MD_KEY", "LIMIT_MD_KEY", "TOTAL_MD_KEY", "NEXT_MD_KEY", "PREV_MD_KEY", "MD_KEYS",
    "POSITION_KEYS", "forward", "forwarded", "relay_position", "response_headers", "with_response_headers",
]

BOUNDARIES_MD_KEY = "x-w17-paging-boundaries"
LIMIT_MD_KEY = "x-w17-paging-limit"
TOTAL_MD_KEY = "x-w17-paging-total"
MD_KEYS = (BOUNDARIES_MD_KEY, LIMIT_MD_KEY, TOTAL_MD_KEY)

# The page's POSITION, reported by the storage handler of a paged query as
# RESPONSE headers (sdk/go lib/paging/position.go): the keyset boundary of the
# page's last row (a forward cursor resumes after it), of its first row (a
# backward one resumes before it), and the total. A call made with forward(ctx)
# relays them onto the business method's own response, so the gateway builds the
# next and previous cursor from where storage says the page ends — the business
# tier never learns how the page is sorted.
NEXT_MD_KEY = "x-w17-paging-next"
PREV_MD_KEY = "x-w17-paging-prev"
POSITION_KEYS = (NEXT_MD_KEY, PREV_MD_KEY, TOTAL_MD_KEY)

_FORWARDED = object()
_RESPONSE_HEADERS = object()


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
    return ctx.with_outgoing(out).with_value(_FORWARDED, True)


def forwarded(ctx: Context) -> bool:
    """Whether ctx came from forward(): its call serves the page."""
    return bool(ctx.value(_FORWARDED, False))


def with_response_headers(ctx: Context) -> tuple[Context, dict[str, tuple[str, ...]]]:
    """The server side: a context whose calls may add response headers to the
    method's answer, and the dict they land in (sent as the initial metadata)."""
    sink: dict[str, tuple[str, ...]] = {}
    return ctx.with_value(_RESPONSE_HEADERS, sink), sink


def response_headers(ctx: Context) -> dict[str, tuple[str, ...]] | None:
    return ctx.value(_RESPONSE_HEADERS)


def relay_position(ctx: Context, header) -> None:
    """Copy the page position a forwarded call answered with (its response
    headers) onto the method's own response headers. A call not made with
    forward(ctx) relays nothing, so a sibling call cannot move the page."""
    if not forwarded(ctx) or header is None:
        return
    sink = response_headers(ctx)
    if sink is None:
        return
    for k in POSITION_KEYS:
        vals = header.get_all(k) if hasattr(header, "get_all") else tuple(header.get(k, ()))
        if vals:
            sink[k] = tuple(vals)
