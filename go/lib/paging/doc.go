// Package paging owns the runtime side of cursor-style REST
// pagination (REV-148).
//
// Three layers compose the feature:
//
//   - Gateway codegen emits a per-endpoint handler that calls
//     EncodeCursor / DecodeCursor at the HTTP boundary and
//     plumbs boundary values onto context.
//   - Storage codegen emits a null-guarded keyset WHERE
//     clause that reads boundary values out of the context
//     using FromContext.
//   - Runtime (this package) carries the encode/decode +
//     context plumbing + keyset comparison helpers used by
//     both sides at runtime.
//
// The cursor wire format is base64-url-safe
// (`base64.RawURLEncoding`) of a `w17.PageCursor` proto. The
// PageCursor proto evolves freely between server builds; a
// `schema_version` field gates breaking evolutions. Clients
// receive `InvalidArgument: cursor_expired` when their cursor
// was emitted by an incompatible build and must refetch from
// page one.
//
// Spec: docs/specs/gateway/cursor-paging.md
// ADR:  docs/decisions/cursor-paging.md
package paging
