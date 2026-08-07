package paging

import (
	"context"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// boundaryCtxKey is the typed context key carrying keyset
// boundary values from the gateway handler down into the
// storage method's emitted SQL. Mirrors the REV-147
// RequestMetadata pattern: the gateway extracts boundaries
// from the cursor, attaches them to ctx; storage codegen
// reads them with FromContext and threads them as SQL
// parameters in the keyset WHERE clause.
//
// Non-paged callers (cross-domain gRPC, batch jobs) do not
// set this key; storage codegen reads nil and the
// null-guarded WHERE clause collapses to no-op.
type boundaryCtxKey struct{}

// Boundaries carries the keyset boundary values + the
// direction the cursor is walking. Direction is needed by
// the storage SQL emit to flip comparison operators on a
// backward resume.
type Boundaries struct {
	Values    []*w17pb.KeysetValue
	Direction w17pb.Direction
}

// WithBoundaries returns a derived context carrying the
// given boundaries. Called by the gateway handler after
// decoding a cursor; the value is read out by the storage
// codegen's emitted SQL parameter-binding code.
//
// Passing nil `b` (or an empty Values slice) is the unpaged
// path: subsequent FromContext returns (nil, false) and the
// storage method's null-guarded keyset WHERE collapses to a
// no-op.
func WithBoundaries(ctx context.Context, b *Boundaries) context.Context {
	if b == nil || len(b.Values) == 0 {
		return ctx
	}
	return context.WithValue(ctx, boundaryCtxKey{}, b)
}

// FromContext extracts boundaries set by WithBoundaries OR
// carried by incoming gRPC metadata.
//
// Two production paths converge here:
//
//   - Tests + direct in-process callers set boundaries via
//     WithBoundaries (context.Value).
//   - The REST gateway threads boundaries via gRPC metadata
//     (key paging.BoundariesMDKey); the storage handler reads
//     them out of `metadata.FromIncomingContext(ctx)` here.
//
// Context-value path wins when both are present (ergonomic
// for tests that want to override metadata).
func FromContext(ctx context.Context) (*Boundaries, bool) {
	if b, ok := ctx.Value(boundaryCtxKey{}).(*Boundaries); ok {
		return b, true
	}
	return BoundariesFromIncomingMD(ctx)
}
