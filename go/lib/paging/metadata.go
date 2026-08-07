package paging

import (
	"context"
	"encoding/base64"
	"strconv"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// BoundariesMDKey is the gRPC metadata key the gateway uses
// to forward cursor-decoded paging boundaries to the storage
// service. Lowercase per gRPC convention (canonical mixed-case
// is normalised by the grpc-go runtime).
//
// The header value is `base64.RawURLEncoding(MarshalledBoundaryList)`
// — a small proto envelope carrying the boundary value list
// + direction.
const BoundariesMDKey = "x-w17-paging-boundaries"

// LimitMDKey is the gRPC metadata key the gateway uses to
// forward the gateway-clamped page-size value to storage.
// The header value is the limit as a base-10 ASCII integer
// (no padding). Storage's emitted SQL uses it as the LIMIT
// argument via the `__paging_limit` scope param.
//
// Absent header → storage applies the DQL's own LIMIT (if
// the author wrote one) OR no LIMIT (returns every row); the
// gateway only omits the header on first-page boot when the
// PagedConfig has zero default_limit AND the request omits
// the limit query parameter — a configuration that opts the
// endpoint into "no clamp" semantics.
const LimitMDKey = "x-w17-paging-limit"

// TotalMDKey carries the cached row count from a resumed
// cursor (iter-2 perf optimisation). When the gateway decodes
// a non-empty ?cursor=, it threads the cursor's `total` field
// to storage via this header; the storage handler then sets
// `resp.Paging.Total = <header>` and SKIPS the COUNT(*) query
// it would otherwise run for first-page requests.
//
// Absent header (first page, no cursor) → storage runs
// COUNT(*) over the filter as before. Iter-1 ran COUNT on
// every page; iter-2 lifts that to "once per pagination
// session, cached in the cursor."
const TotalMDKey = "x-w17-paging-total"

// boundaryListEnvelope is the wire shape of the metadata
// header value. Reuses w17pb types (KeysetValue, Direction)
// rather than introducing a third proto file just for this
// internal envelope.
//
// Since we want to evolve this independently of PageCursor
// (which carries the request bytes + total + schema_version
// alongside boundaries), we serialise as a discrete proto
// using a tiny manual struct → bytes mapping.
//
// In practice we marshal a PageCursor with empty Request +
// total + schema_version, populated only Direction +
// Boundaries fields. Receivers MUST ignore the empty fields
// because the value is purely a boundary carrier here.

// EncodeBoundariesMD returns the base64-url-safe value the
// gateway sets under `BoundariesMDKey` in its outgoing gRPC
// metadata. `nil` boundaries → empty string (caller should
// skip the metadata.AppendToOutgoingContext call entirely in
// that case).
func EncodeBoundariesMD(boundaries []*w17pb.KeysetValue, direction w17pb.Direction) (string, error) {
	if len(boundaries) == 0 {
		return "", nil
	}
	env := &w17pb.PageCursor{
		Boundaries: boundaries,
		Direction:  direction,
	}
	b, err := proto.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeBoundariesMD reverses EncodeBoundariesMD. Returns
// `(nil, false)` when the value is empty or malformed —
// callers treat both as "no boundaries this request".
func DecodeBoundariesMD(value string) (*Boundaries, bool) {
	if value == "" {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	env := &w17pb.PageCursor{}
	if err := proto.Unmarshal(raw, env); err != nil {
		return nil, false
	}
	if len(env.GetBoundaries()) == 0 {
		return nil, false
	}
	// Every boundary must carry a set oneof. A crafted metadata header
	// whose KeysetValue oneof is empty (valid proto wire) would otherwise
	// reach ScalarOf in emitted storage code and panic. The client-facing
	// cursor path guards this in DecodeCursor; mirror it here for the
	// separate gateway→storage metadata-boundary decoder. Treat as
	// malformed → "no boundaries this request" per this decoder's contract.
	for _, b := range env.GetBoundaries() {
		if b.GetValue() == nil {
			return nil, false
		}
	}
	return &Boundaries{Values: env.GetBoundaries(), Direction: env.GetDirection()}, true
}

// BoundariesFromIncomingMD pulls boundaries out of the
// incoming gRPC metadata attached to `ctx`. Returns
// `(nil, false)` when the metadata header is absent or
// malformed.
//
// Used as a fallback inside FromContext when no context-value
// boundaries are set — production callers (the REST gateway)
// thread boundaries via metadata; tests + direct in-process
// callers may use WithBoundaries + ctx values instead. Both
// paths converge at FromContext.
func BoundariesFromIncomingMD(ctx context.Context) (*Boundaries, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false
	}
	vals := md.Get(BoundariesMDKey)
	if len(vals) == 0 {
		return nil, false
	}
	return DecodeBoundariesMD(vals[0])
}

// EncodeLimitMD renders the gateway-clamped limit as the
// metadata header value (base-10 ASCII int). Zero → empty
// (caller skips the header).
func EncodeLimitMD(limit uint32) string {
	if limit == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(limit), 10)
}

// LimitFromIncomingMD reads the gateway-clamped page size
// from incoming gRPC metadata. Returns `(0, false)` when the
// header is absent OR malformed — storage's emitted query
// then falls back to its DQL-declared LIMIT (or no LIMIT at
// all when the DQL omits one).
func LimitFromIncomingMD(ctx context.Context) (uint32, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, false
	}
	vals := md.Get(LimitMDKey)
	if len(vals) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(vals[0], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// EncodeTotalMD renders the cached total as the metadata
// header value (base-10 ASCII uint64). Zero → empty string
// (caller skips the header — zero total is the "no cached
// value" sentinel, distinct from "filter has zero matches"
// which the cursor would not have been emitted for in the
// first place).
func EncodeTotalMD(total uint64) string {
	if total == 0 {
		return ""
	}
	return strconv.FormatUint(total, 10)
}

// TotalFromIncomingMD reads the cached total from incoming
// gRPC metadata. Returns `(0, false)` when the header is
// absent or malformed — storage then runs the COUNT(*) query
// as the iter-1 path did.
func TotalFromIncomingMD(ctx context.Context) (uint64, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, false
	}
	vals := md.Get(TotalMDKey)
	if len(vals) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(vals[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
