package fileref

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A download of a gated bucket serves a file to the readers of the row that
// references it. So the reference itself must not be something a client can
// simply write: a caller who learned another organization's key (a pasted
// link, a referer, an ex-member's notes) would put it into a row of his own
// and read the file through that row. The gateway therefore lets a request
// carry a handle in an upload field of a gated bucket only when
//
//   - the gateway minted it in this very request (the multipart upload —
//     [WithMinted]); or
//   - it is empty (clearing the field); or
//   - the caller may already read that file: a row the caller may read
//     references it ([Readable], through the caller's own scopes and the
//     request's range). That is the row's current value sent back on an
//     update, or a file the caller could have downloaded and uploaded
//     again anyway — nothing the caller could not reach before.
//
// Anything else is refused (InvalidArgument), whatever surface carried it:
// a JSON body, a gRPC request, an MCP tool call.

// Claim is one field of a message that can carry an upload handle: an
// uploaded string field of a gated bucket (Message false), or a message
// field whose type carries such fields further down (Message true).
type Claim struct {
	Field   protoreflect.Name
	Message bool
}

// Claims are the claimable fields of every message a gateway sends that
// can carry one, by the message's full name. Codegen writes it.
type Claims map[protoreflect.FullName][]Claim

type mintedKey struct{}

// WithMinted records the handles the gateway itself stored in this request
// (a multipart upload): they are the request's own, not claims.
func WithMinted(ctx context.Context, handles ...string) context.Context {
	if len(handles) == 0 {
		return ctx
	}
	prev, _ := ctx.Value(mintedKey{}).(map[string]bool)
	next := make(map[string]bool, len(prev)+len(handles))
	for h := range prev {
		next[h] = true
	}
	for _, h := range handles {
		next[h] = true
	}
	return context.WithValue(ctx, mintedKey{}, next)
}

func minted(ctx context.Context, h string) bool {
	m, _ := ctx.Value(mintedKey{}).(map[string]bool)
	return m[h]
}

// GuardClaims wraps a gateway's backend conns so that every request sent
// through them is checked against claims before it leaves: a handle in a
// claimable field that the gateway did not mint, that is not empty and
// that the caller may not already read is refused. refs is the domain's
// storage tier (it serves w17.FileRefs); the check rides the request's own
// context, so it carries the caller's scopes and the method's range.
func GuardClaims(storage, business grpc.ClientConnInterface, claims Claims) (grpc.ClientConnInterface, grpc.ClientConnInterface) {
	if len(claims) == 0 {
		return storage, business
	}
	g := &guard{refs: storage, claims: claims}
	gs := &claimConn{ClientConnInterface: storage, g: g}
	if business == storage {
		return gs, gs
	}
	return gs, &claimConn{ClientConnInterface: business, g: g}
}

type guard struct {
	refs   grpc.ClientConnInterface
	claims Claims
}

type claimConn struct {
	grpc.ClientConnInterface
	g *guard
}

func (c *claimConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if err := c.g.check(ctx, args); err != nil {
		return err
	}
	return c.ClientConnInterface.Invoke(ctx, method, args, reply, opts...)
}

func (c *claimConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cs, err := c.ClientConnInterface.NewStream(ctx, desc, method, opts...)
	if err != nil {
		return nil, err
	}
	return &claimStream{ClientStream: cs, ctx: ctx, g: c.g}, nil
}

type claimStream struct {
	grpc.ClientStream
	ctx context.Context
	g   *guard
}

func (s *claimStream) SendMsg(m any) error {
	if err := s.g.check(s.ctx, m); err != nil {
		return err
	}
	return s.ClientStream.SendMsg(m)
}

// check refuses the first handle in msg the caller may not claim.
func (g *guard) check(ctx context.Context, msg any) error {
	pm, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	var found []claimed
	g.collect(pm.ProtoReflect(), "", &found, 0)
	for _, c := range found {
		if c.handle == "" || minted(ctx, c.handle) {
			continue
		}
		readable, err := Readable(ctx, g.refs, c.handle)
		if err != nil {
			switch status.Code(err) {
			case codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
				readable = false
			default:
				return err
			}
		}
		if !readable {
			return status.Errorf(codes.InvalidArgument, "%s: not a file this request may reference — a file is attached by uploading it", c.path)
		}
	}
	return nil
}

type claimed struct {
	path   string
	handle string
}

// maxClaimDepth bounds the walk over a self-referencing message.
const maxClaimDepth = 32

func (g *guard) collect(m protoreflect.Message, prefix string, out *[]claimed, depth int) {
	if depth > maxClaimDepth || !m.IsValid() {
		return
	}
	for _, c := range g.claims[m.Descriptor().FullName()] {
		fd := m.Descriptor().Fields().ByName(c.Field)
		if fd == nil || !m.Has(fd) {
			continue
		}
		path := prefix + string(c.Field)
		v := m.Get(fd)
		switch {
		case fd.IsMap():
			if c.Message && fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					g.collect(mv.Message(), path+".", out, depth+1)
					return true
				})
			}
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if c.Message && fd.Kind() == protoreflect.MessageKind {
					g.collect(l.Get(i).Message(), path+".", out, depth+1)
				} else if fd.Kind() == protoreflect.StringKind {
					*out = append(*out, claimed{path: path, handle: l.Get(i).String()})
				}
			}
		case c.Message && fd.Kind() == protoreflect.MessageKind:
			g.collect(v.Message(), path+".", out, depth+1)
		case fd.Kind() == protoreflect.StringKind:
			*out = append(*out, claimed{path: path, handle: v.String()})
		}
	}
}
