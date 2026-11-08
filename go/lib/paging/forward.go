package paging

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// mdKeys are the paging keys a gateway stamps for one paged request. They
// describe ONE page and travel as one set.
var mdKeys = [...]string{BoundariesMDKey, LimitMDKey, TotalMDKey}

// Forward returns a context that carries the INCOMING paging metadata
// (`x-w17-paging-boundaries`, `-limit`, `-total`) on its OUTGOING side, for a
// business method that serves a paged endpoint by calling a paged storage
// query.
//
// Nothing forwards these keys automatically, and that is deliberate:
// [principal.ForwardToOutgoing] leaves them alone, because one query's LIMIT
// and keyset would bleed onto every sibling storage call the method makes.
// Without this, the storage query answered the first page at its default size
// whatever cursor the caller sent. Pass the returned context to the call (or
// the few calls) that serve the page and to nothing else, then copy the
// storage response's w17.Paging into your own response:
//
//	res, err := s.clients.NoteQuery().ListNotes(paging.Forward(ctx), req)
//
// The three keys are replaced as a set: when the incoming side carries any of
// them, the outgoing side's copies of all three are dropped first, so a
// first-page request (a limit, no boundaries) does not go out with boundaries
// left on a reused context. Every other outgoing key is kept. With none
// incoming, ctx is returned unchanged.
//
// The returned context is also MARKED: the business tier's storage
// connection ([RelayPositionUnaryInterceptor], and inprocgrpc in a composed
// binary) copies the page position the call answers with (NextMDKey,
// PrevMDKey, TotalMDKey response headers) onto this method's own response
// headers, so the gateway builds the next and previous cursor from where
// storage says the page ends without the business tier knowing how it is
// sorted. A call made without Forward relays nothing.
//
// The Python, TypeScript and Rust business SDKs carry the same function
// (`paging.forward(ctx)`), checked by the sdk's conformance suite.
func Forward(ctx context.Context) context.Context {
	in, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	paged := metadata.MD{}
	for _, k := range mdKeys {
		if vals := in.Get(k); len(vals) > 0 {
			paged[k] = append([]string(nil), vals...)
		}
	}
	if len(paged) == 0 {
		return ctx
	}
	out, _ := metadata.FromOutgoingContext(ctx)
	next := out.Copy()
	if next == nil {
		next = metadata.MD{}
	}
	for _, k := range mdKeys {
		next.Delete(k)
	}
	for k, v := range paged {
		next[k] = v
	}
	return context.WithValue(metadata.NewOutgoingContext(ctx, next), forwardedKey{}, true)
}

type forwardedKey struct{}

// Forwarded reports whether ctx came from [Forward]: its call serves the
// page, so its response's page position is relayed.
func Forwarded(ctx context.Context) bool {
	v, _ := ctx.Value(forwardedKey{}).(bool)
	return v
}

// WithoutForward drops the [Forward] mark: a server context built from a
// forwarded call (inprocgrpc) must not relay its own calls' positions onto
// the caller's response.
func WithoutForward(ctx context.Context) context.Context {
	if !Forwarded(ctx) {
		return ctx
	}
	return context.WithValue(ctx, forwardedKey{}, false)
}
