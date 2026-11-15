// Package fileref answers the download gate's one question — does a record
// the caller may read reference this stored file? — across the gateway →
// storage hop.
//
// A file uploaded to (or written by a business tier into) a bucket that
// holds a scoped model's files belongs to the row whose column holds its
// key. The gateway cannot see that row; the storage tier can, and it
// already knows how to read a model through the caller's data scopes (the
// `x-w17-scope-<name>` metadata + `x-w17-data-scope` range the gateway
// threads). So the generated storage bundle serves `w17.FileRefs/Readable`
// over [Ref]s — one per uploaded column of the domain's models — and the
// generated gateway asks it ([Readable]) before it streams an object of a
// gated bucket.
//
// The answer follows the RECORD, not the writer: a staff member's
// attachment to alice's row is alice's to download, and an object stored
// before the gate existed (or by a job, a subscriber, a non-Go business
// tier) is served to its record's readers exactly like any other.
package fileref

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// ServiceName is the gRPC service a storage bundle registers when one of
// its models stores files in a bucket a download gates.
const ServiceName = "w17.FileRefs"

// readableMethod is the full method name of [Server.Readable].
const readableMethod = "/" + ServiceName + "/Readable"

// Server is the storage side: Readable answers whether a record the caller
// may read references the object key in the request.
type Server interface {
	Readable(ctx context.Context, key *wrapperspb.StringValue) (*wrapperspb.BoolValue, error)
}

// RegisterW17FileRefsServer registers srv on reg (a *grpc.Server or a
// composed binary's in-process registrar).
func RegisterW17FileRefsServer(reg grpc.ServiceRegistrar, srv Server) {
	reg.RegisterService(&serviceDesc, srv)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*Server)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Readable",
		Handler:    readableHandler,
	}},
	Streams:  []grpc.StreamDesc{},
	Metadata: "w17/fileref",
}

func readableHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(wrapperspb.StringValue)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Server).Readable(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: readableMethod}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Server).Readable(ctx, req.(*wrapperspb.StringValue))
	})
}

// Readable asks the storage tier behind conn whether a record the caller
// may read references key. ctx carries the caller's data scopes as
// outgoing metadata (`x-w17-scope-<name>`, `x-w17-data-scope`) exactly as
// for any other call the gateway makes on the caller's behalf.
func Readable(ctx context.Context, conn grpc.ClientConnInterface, key string) (bool, error) {
	out := new(wrapperspb.BoolValue)
	if err := conn.Invoke(ctx, readableMethod, wrapperspb.String(key), out); err != nil {
		return false, err
	}
	return out.GetValue(), nil
}

// Querier is what a [Ref] reads through: a *sql.DB.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Arg is one bound parameter of a [Ref]'s query, in placeholder order.
type Arg struct {
	// Scope names the data scope whose caller value binds here; empty = the
	// object key.
	Scope string
	// Owner marks an OWNER-axis scope: the caller's organization range
	// (`x-w17-data-scope` "org" or "all") lifts it — it binds NULL, which
	// the query's `(<col> = ? OR ? IS NULL)` reads as "any owner" — exactly
	// as the model's generated reads lift it.
	Owner bool
}

// KeyArg binds the object key.
func KeyArg() Arg { return Arg{} }

// ScopeArg binds the caller's value of an organization-axis scope.
func ScopeArg(name string) Arg { return Arg{Scope: name} }

// OwnerArg binds the caller's value of an owner-axis scope (NULL when the
// request's range lifts the owner axis).
func OwnerArg(name string) Arg { return Arg{Scope: name, Owner: true} }

// Ref is one uploaded column of one model: Query returns a row when a row
// of that model whose column holds the bound key is readable by the bound
// scopes.
type Ref struct {
	// Bucket is the column's bucket path without its leading '/': only a
	// key under it can be this column's (a cheap filter, not the gate).
	Bucket string
	// DB is the connection the model's table lives on.
	DB Querier
	// Query is the dialect's SELECT; Args bind its placeholders in order.
	Query string
	Args  []Arg
}

// NewServer serves Readable over refs.
func NewServer(refs ...Ref) Server { return &server{refs: refs} }

type server struct{ refs []Ref }

// Readable reports true when any ref's query finds a row. A ref that needs
// a scope the caller does not carry (absent, empty, or ambiguous) is
// skipped — a missing value never matches anything, on either side: the
// caller's is never bound as "", and a row whose scope column is NULL or
// "" equals no caller's value.
func (s *server) Readable(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.BoolValue, error) {
	key := in.GetValue()
	if key == "" {
		return wrapperspb.Bool(false), nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	lifted := false
	switch principal.DataScope(ctx) {
	case principal.DataScopeOrg, principal.DataScopeAll:
		lifted = true
	}
	for _, ref := range s.refs {
		if ref.Bucket != "" && !strings.HasPrefix(key, ref.Bucket+"/") {
			continue
		}
		args, ok := bind(ref.Args, key, md, lifted)
		if !ok {
			continue
		}
		var one int
		err := ref.DB.QueryRowContext(ctx, ref.Query, args...).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fileref: %v", err)
		}
		return wrapperspb.Bool(true), nil
	}
	return wrapperspb.Bool(false), nil
}

// bind resolves args against the caller's metadata; false when one of
// them needs a scope the caller does not carry exactly once.
func bind(args []Arg, key string, md metadata.MD, lifted bool) ([]any, bool) {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if a.Scope == "" {
			out = append(out, key)
			continue
		}
		// Required even where the range lifts it, as the model's own
		// generated reads require it: a caller without the scope reads
		// nothing of the model.
		vals := md.Get(principal.ScopeKey(a.Scope))
		if len(vals) != 1 || vals[0] == "" {
			return nil, false
		}
		if a.Owner && lifted {
			out = append(out, nil)
			continue
		}
		out = append(out, vals[0])
	}
	return out, true
}
