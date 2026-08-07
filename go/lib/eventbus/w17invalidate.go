package eventbus

import (
	"context"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// W17InvalidateTopic is the reserved eventbus topic name the
// gateway's /w17-events SSE route forwards as the project-wide
// cache-invalidation signal (REV-140 / C4.4). Pinned by spec
// — see docs/specs/gateway/w17-events-channel.md §Reserved
// built-in topic.
const W17InvalidateTopic = "w17.invalidate"

// EmitInvalidate is the convenience wrapper around
// Dispatcher.Dispatch for the reserved w17.invalidate topic.
// Drops FE caches tagged with the touched entity family —
// both page-local (handler's own page) and cross-tab (every
// other open client on the same channel).
//
// **Two call paths.** Both produce identical wire frames; pick
// based on whether the mutation's proto carries
// (w17.touches_entities):
//
//  1. **Auto-emit (REV-145 / C4.6, preferred).** Annotate the
//     mutation's proto method with `option (w17.touches_entities)
//     = "Account";` (repeated for multi-entity mutations). The
//     storage codegen pipeline walks the annotation at build time
//     and wires the call into the generated EmitInterceptor — no
//     handler-body code change. Channel is pinned to "default";
//     ids are passed as nil (matches the FE's family-level drop
//     behavior today).
//
//  2. **Manual (still supported).** Call from operator handler
//     bodies when the auto-emit path doesn't fit (e.g. domains
//     intentionally without proto annotations, or callers wanting
//     to populate the ids list for observability):
//
//     if err := eventbus.EmitInvalidate(ctx, bus, "default", "accounts", "Account", []string{updated.Id}); err != nil {
//     log.Warn().Err(err).Msg("invalidate signal dropped")
//     }
//
// **Labels come from ctx, not from the caller.** The reserved topic is
// dispatched as a bare InvalidateEvent rather than inside the per-domain
// Event envelope, so it had no label channel and every invalidate went out
// unlabelled — which an isolating /w17-events hub withholds from every
// connected principal (restgw.entitled). Cache invalidation reached NOBODY
// the moment principals gained labels, while the generated transcode's
// comment claimed the opposite. The labels are read off the emitting
// request's context with the same shared rule every other event uses
// ([LabelsFromContext]), so no call site has to pass anything: an emit
// inside a labelled request is labelled, an emit outside one is not.
//
// **Ids are informational** — the FE client today drops by
// (group, message) family without consulting the id list.
// Populate them anyway on the manual path: future polish may
// narrow drops to specific ids when populated, and the values
// surface in observability (which entities triggered the
// invalidate). The auto-emit path passes nil because the
// generator has no shape-aware response field extractor yet.
func EmitInvalidate(ctx context.Context, bus Dispatcher, channel, group, message string, ids []string) error {
	return bus.Dispatch(ctx, channel, W17InvalidateTopic, &w17pb.InvalidateEvent{
		Group:   group,
		Message: message,
		Ids:     ids,
		Labels:  LabelsFromContext(ctx),
	})
}
