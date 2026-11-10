//! Paging, forwarded by hand.
//!
//! The gateway turns a paged request's cursor and page size into
//! `x-w17-paging-{boundaries,limit,total}` metadata. A business method bound
//! to a paged endpoint receives them INCOMING; the storage query it calls
//! reads them incoming on ITS side. Nothing copies them across the hop
//! automatically, and that is deliberate
//! ([`crate::principal::forward_to_outgoing`] leaves them alone): one query's
//! LIMIT and keyset would bleed onto every sibling storage call the method
//! makes.
//!
//! [`forward`] is the explicit copy. Pass what it returns to the ONE call (or
//! the few calls) that serve the page, and nothing else; then copy the
//! storage response's `w17.Paging` into your own response. The page position
//! that call answers with (`x-w17-paging-next` / `-prev` / `-total`, response
//! headers) comes back on the method's own response headers, so the gateway
//! builds the next cursor from where storage says the page ends. The Go twin
//! is `paging.Forward`.
//!
//! Never pass it to `registry.self_channel()`: a composed call hands the
//! callee this context unchanged, mark included, so the callee's own storage
//! calls would relay their position onto your response.

use http::HeaderMap;

use crate::{ctx::Ctx, metadata::Metadata};

pub const BOUNDARIES_MD_KEY: &str = "x-w17-paging-boundaries";
pub const LIMIT_MD_KEY: &str = "x-w17-paging-limit";
pub const TOTAL_MD_KEY: &str = "x-w17-paging-total";
pub const MD_KEYS: &[&str] = &[BOUNDARIES_MD_KEY, LIMIT_MD_KEY, TOTAL_MD_KEY];

/// The page's POSITION, reported by the storage handler of a paged query as
/// RESPONSE headers: the keyset boundary of the page's last row, of its first
/// row, and the total.
pub const NEXT_MD_KEY: &str = "x-w17-paging-next";
pub const PREV_MD_KEY: &str = "x-w17-paging-prev";
pub const POSITION_KEYS: &[&str] = &[NEXT_MD_KEY, PREV_MD_KEY, TOTAL_MD_KEY];

/// Copy the incoming paging keys onto the outgoing side.
///
/// The three keys travel as ONE set, as they describe one request's page:
/// when the incoming side carries any of them, the outgoing copies of all
/// three are replaced — so a first-page request (a limit, no boundaries)
/// does not go out with boundaries left on a reused context. With none
/// incoming the context is returned unchanged. Values are copied verbatim,
/// every value of a key.
pub fn forward(ctx: &Ctx) -> Ctx {
    let incoming: Metadata = MD_KEYS
        .iter()
        .flat_map(|k| {
            ctx.incoming()
                .get_all(k)
                .iter()
                .map(move |v| (*k, v.clone()))
        })
        .collect();
    if incoming.is_empty() {
        return ctx.clone();
    }
    let out = ctx
        .outgoing()
        .without(|k| MD_KEYS.contains(&k))
        .merged(&incoming);
    ctx.with_outgoing(out).with_paging_forwarded()
}

/// Whether `ctx` came from [`forward`]: its call serves the page.
pub fn forwarded(ctx: &Ctx) -> bool {
    ctx.paging_forwarded()
}

/// Copy the page position a forwarded call answered with (its response
/// headers) onto the method's own response headers. A call not made with
/// [`forward`] relays nothing, so a sibling call cannot move the page.
pub(crate) fn relay_position(ctx: &Ctx, headers: &HeaderMap) {
    if !forwarded(ctx) {
        return;
    }
    let Some(sink) = ctx.response_headers() else {
        return;
    };
    let mut sink = sink.lock().unwrap_or_else(|e| e.into_inner());
    for k in POSITION_KEYS {
        let vals: Vec<String> = headers
            .get_all(*k)
            .iter()
            .filter_map(|v| v.to_str().ok().map(str::to_string))
            .collect();
        if !vals.is_empty() {
            *sink = sink.with_set(k, vals);
        }
    }
}
