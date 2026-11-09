/**
 * Paging, forwarded by hand.
 *
 * The gateway turns a paged request's cursor and page size into
 * `x-w17-paging-{boundaries,limit,total}` metadata. A business method bound to
 * a paged endpoint receives them INCOMING; the storage query it calls reads
 * them incoming on ITS side. Nothing copies them across the hop
 * automatically, and that is deliberate (`principal.forwardToOutgoing` leaves
 * them alone): one query's LIMIT and keyset would bleed onto every sibling
 * storage call the method makes.
 *
 * `forward(ctx)` is the explicit copy. Pass what it returns to the ONE call
 * (or the few calls) that serve the page, and nothing else; then copy the
 * storage response's `w17.Paging` into your own response. The Go twin is
 * `paging.Forward` in sdk/go/lib/paging.
 *
 * Never pass it to `registry.self(...)`: a composed call hands the callee
 * this context unchanged, as Go's direct `s.self` call does, mark included,
 * so the callee's own storage calls would relay their position onto your
 * response.
 */

import type { Context, Metadata } from "./context.js";

export const BOUNDARIES_MD_KEY = "x-w17-paging-boundaries";
export const LIMIT_MD_KEY = "x-w17-paging-limit";
export const TOTAL_MD_KEY = "x-w17-paging-total";
export const MD_KEYS: readonly string[] = [BOUNDARIES_MD_KEY, LIMIT_MD_KEY, TOTAL_MD_KEY];

/**
 * The page's POSITION, reported by the storage handler of a paged query as
 * RESPONSE headers (sdk/go lib/paging/position.go): the keyset boundary of
 * the page's last row (a forward cursor resumes after it), of its first row
 * (a backward one resumes before it), and the total. A call made with
 * forward(ctx) relays them onto the business method's own response, so the
 * gateway builds the next and previous cursor from where storage says the
 * page ends — the business tier never learns how the page is sorted.
 */
export const NEXT_MD_KEY = "x-w17-paging-next";
export const PREV_MD_KEY = "x-w17-paging-prev";
export const POSITION_KEYS: readonly string[] = [NEXT_MD_KEY, PREV_MD_KEY, TOTAL_MD_KEY];

const FORWARDED = Symbol("w17.paging.forwarded");
const RESPONSE_HEADERS = Symbol("w17.paging.responseHeaders");

/** Response headers a method answers with, key → values. */
export type HeaderSink = Map<string, readonly string[]>;

/**
 * Copy the incoming paging keys onto the outgoing side.
 *
 * The three keys travel as ONE set, as they describe one request's page:
 * when the incoming side carries any of them, the outgoing copies of all
 * three are replaced — so a first-page request (a limit, no boundaries) does
 * not go out with boundaries left on a reused context. With none incoming
 * the context is returned unchanged. Values are copied verbatim, every value
 * of a key.
 */
export function forward(ctx: Context): Context {
  const incoming = new Map<string, readonly string[]>();
  for (const k of MD_KEYS) {
    const v = ctx.incoming.getAll(k);
    if (v.length) incoming.set(k, v);
  }
  if (!incoming.size) return ctx;
  const out = ctx.outgoing.without((k) => MD_KEYS.includes(k)).merged(incoming);
  return ctx.withOutgoing(out).withValue(FORWARDED, true);
}

/** Whether ctx came from forward(): its call serves the page. */
export function forwarded(ctx: Context): boolean {
  return ctx.value(FORWARDED) === true;
}

/**
 * The server side: a context whose calls may add response headers to the
 * method's answer, and the map they land in (sent as the initial metadata).
 */
export function withResponseHeaders(ctx: Context): [Context, HeaderSink] {
  const sink: HeaderSink = new Map();
  return [ctx.withValue(RESPONSE_HEADERS, sink), sink];
}

export function responseHeaders(ctx: Context): HeaderSink | undefined {
  return ctx.value<HeaderSink>(RESPONSE_HEADERS);
}

/**
 * Copy the page position a forwarded call answered with (its response
 * headers) onto the method's own response headers. A call not made with
 * forward(ctx) relays nothing, so a sibling call cannot move the page.
 */
export function relayPosition(ctx: Context, header: Metadata | undefined): void {
  if (!forwarded(ctx) || header === undefined) return;
  const sink = responseHeaders(ctx);
  if (sink === undefined) return;
  for (const k of POSITION_KEYS) {
    const vals = header.getAll(k);
    if (vals.length) sink.set(k, [...vals]);
  }
}
