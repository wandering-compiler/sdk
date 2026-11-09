package paging

import (
	"context"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The page's POSITION travels back as RESPONSE header metadata, so a paged
// endpoint can be served through a hop that does not know how the page is
// sorted (a business method that forwarded it to storage with [Forward]).
//
//   - NextMDKey — the keyset boundary a FORWARD cursor resumes after: the
//     sort-key values of the page's LAST row, in the order the client sees
//     the rows (a backward page is reversed first).
//   - PrevMDKey — the boundary a BACKWARD cursor resumes before: the page's
//     FIRST row.
//   - TotalMDKey — the page's total, as the response's w17.Paging carries
//     it (the same key the gateway sends the cached total to storage on).
//
// The boundary values use the encoding of BoundariesMDKey
// ([EncodeBoundariesMD]), with the direction a cursor built from them takes.
// The storage handler of a paged query sets them ([SetPosition]); the gateway
// reads them ([PositionFromHeader]) for an endpoint whose method has no DQL
// of its own; a business tier relays them from the call it forwarded the page
// to ([RelayPositionUnaryInterceptor]).
const (
	NextMDKey = "x-w17-paging-next"
	PrevMDKey = "x-w17-paging-prev"
)

// positionKeys are the response header keys of one page's position.
var positionKeys = [...]string{NextMDKey, PrevMDKey, TotalMDKey}

// Position is a page's position as its response headers report it.
type Position struct {
	// Next and Prev are the boundaries of the last / first row; nil when the
	// page had no rows (or the header was absent or malformed).
	Next, Prev []*w17pb.KeysetValue
	// Total is the reported total; HasTotal says whether one was reported.
	Total    uint64
	HasTotal bool
}

// PositionMD is the header metadata that reports a page's position. Empty
// boundaries are left out.
func PositionMD(next, prev []*w17pb.KeysetValue, total uint64) metadata.MD {
	md := metadata.MD{}
	if v, err := EncodeBoundariesMD(next, w17pb.Direction_DIRECTION_FORWARD); err == nil && v != "" {
		md.Set(NextMDKey, v)
	}
	if v, err := EncodeBoundariesMD(prev, w17pb.Direction_DIRECTION_BACKWARD); err == nil && v != "" {
		md.Set(PrevMDKey, v)
	}
	// A zero total is a total (an empty filter), not the "nothing cached"
	// of the request side's EncodeTotalMD, so it is written out.
	md.Set(TotalMDKey, strconv.FormatUint(total, 10))
	return md
}

// SetPosition reports a page's position on the handler's response headers.
// The generated storage handler of a paged query calls it. A context with no
// server transport (a direct call in a test) has nowhere to set them, and
// nothing is reported: the gateway of a storage-bound endpoint builds its
// cursors from the rows, and only a relayed page needs the headers.
func SetPosition(ctx context.Context, next, prev []*w17pb.KeysetValue, total uint64) {
	_ = grpc.SetHeader(ctx, PositionMD(next, prev, total))
}

// PositionFromHeader reads a page's position from response header metadata.
// ok is false when none of the keys is present.
func PositionFromHeader(md metadata.MD) (Position, bool) {
	var p Position
	ok := false
	if vs := md.Get(NextMDKey); len(vs) > 0 {
		ok = true
		if b, good := DecodeBoundariesMD(vs[len(vs)-1]); good {
			p.Next = b.Values
		}
	}
	if vs := md.Get(PrevMDKey); len(vs) > 0 {
		ok = true
		if b, good := DecodeBoundariesMD(vs[len(vs)-1]); good {
			p.Prev = b.Values
		}
	}
	if vs := md.Get(TotalMDKey); len(vs) > 0 {
		ok = true
		if t, good := totalFromValue(vs[len(vs)-1]); good {
			p.Total, p.HasTotal = t, true
		}
	}
	return p, ok
}

func totalFromValue(v string) (uint64, bool) {
	n, err := strconv.ParseUint(v, 10, 64)
	return n, err == nil
}

// pickPosition returns the position keys of md, nil when it carries none.
func pickPosition(md metadata.MD) metadata.MD {
	var out metadata.MD
	for _, k := range positionKeys {
		if vs := md.Get(k); len(vs) > 0 {
			if out == nil {
				out = metadata.MD{}
			}
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// RelayPosition copies the page position a forwarded call answered with
// (hdr, its response headers) onto the response headers of the method that
// made it, whose server context ctx derives from. A call made without
// [Forward] relays nothing, so a sibling call's page cannot overwrite the
// one the method serves.
func RelayPosition(ctx context.Context, hdr metadata.MD) {
	if !Forwarded(ctx) {
		return
	}
	if pos := pickPosition(hdr); pos != nil {
		_ = grpc.SetHeader(ctx, pos)
	}
}

// RelayPositionUnaryInterceptor is the client interceptor of a business
// tier's storage connection (core/grpcx.DialOpts): for a call made with a
// [Forward] context it collects the response headers and relays the page's
// position onto the business method's own response, which is how the gateway
// learns where the next and previous pages start without knowing how the
// page was sorted. Any other call passes through untouched.
func RelayPositionUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if !Forwarded(ctx) {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	var hdr metadata.MD
	err := invoker(ctx, method, req, reply, cc, append(opts, grpc.Header(&hdr))...)
	if err == nil {
		RelayPosition(ctx, hdr)
	}
	return err
}
