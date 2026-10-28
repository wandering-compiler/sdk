// Package egress is the runtime of a generated egress client: the gRPC
// service `<Name>Api` that `w17ctl client generate` declares for a
// third-party REST API, served in-process, each call an HTTP request.
//
// There is no per-client generated server. Everything a call needs is in the
// client's proto — the options of proto/w17/egress.proto say each method's
// HTTP method and path, where each request field goes and what each JSON key
// is — so one implementation reads them off the service descriptor and
// serves any client: [Register]. The project's generated `egress` package
// only registers each client on a bundle's in-process connection and hands
// out its typed gRPC client.
//
// What a call does: the request message is split onto the path, query,
// headers and body (package wire); the DSN's credential is set; the request
// is sent with the call policy — retries with a capped back-off on 429, 502,
// 503, 504 and transport errors, only for a method a retry cannot double
// (GET, HEAD, PUT, DELETE, OPTIONS, or a POST/PATCH with an idempotency
// header, its key generated once per call), Retry-After honoured within the
// cap, every retry a span event — and the success body is decoded, capped
// at [MaxResponseBytes]. A failure is a gRPC status whose code describes the
// caller's situation and which carries a google.rpc.ErrorInfo (domain = the
// client) with the upstream status, the attempt count and the upstream's
// error body, truncated. The DSN never appears in an error or a span.
//
// A project may serve a client by hand instead: implement `<Name>ApiServer`,
// register it on a connection of its own and point the project's `egress`
// package at that connection (its Use); callers do not change.
package egress

import (
	"context"
	"fmt"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/egress/wire"
)

// Option configures [Register].
type Option func(*options)

type options struct {
	transport http.RoundTripper
}

// WithTransport sends the client's requests through rt instead of a
// transport built from the DSN (whose proxy it then ignores). For tests,
// and for a transport the project instruments itself.
func WithTransport(rt http.RoundTripper) Option { return func(o *options) { o.transport = rt } }

// Name is the egress name of the client svc belongs to: its file's
// (w17.egress_client).name.
func Name(svc protoreflect.ServiceDescriptor) string {
	if c := wire.Client(svc.ParentFile()); c != nil && c.GetName() != "" {
		return c.GetName()
	}
	return string(svc.Name())
}

// Register serves svc on reg, every call an HTTP request to the upstream dsn
// names. It refuses a service that is not an egress client, or one with a
// streaming method, at wiring time. The upstream is not contacted here: an
// unreachable third party must not keep a bundle from starting.
func Register(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, dsn *DSN, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	client := Name(svc)
	c := newCaller(client, dsn, o.transport)
	desc, err := serviceDesc(svc, func(m *method, in, out proto.Message) func(context.Context) error {
		return func(ctx context.Context) error { return c.do(ctx, m, in, out) }
	})
	if err != nil {
		return err
	}
	reg.RegisterService(desc, struct{}{})
	return nil
}

// RegisterUnconfigured serves svc refusing every call with
// FAILED_PRECONDITION naming env: a bundle whose environment carries no DSN
// for the client still links its callers, and a call is how they learn.
func RegisterUnconfigured(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, env string) error {
	client := Name(svc)
	desc, err := serviceDesc(svc, func(*method, proto.Message, proto.Message) func(context.Context) error {
		return func(context.Context) error {
			return callError(codes.FailedPrecondition, client, ReasonUnconfigured,
				"the client is not configured in this bundle: set "+env, map[string]string{"env": env})
		}
	})
	if err != nil {
		return err
	}
	reg.RegisterService(desc, struct{}{})
	return nil
}

// RegisterFromEnv registers svc configured from the env var env, read
// through lookup (the project's secret resolution): [Register] when it is
// set, [RegisterUnconfigured] when it is not. A DSN that is set but does not
// parse is an error — a misconfiguration, reported at boot; the message
// names the variable and never its value.
func RegisterFromEnv(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, env string, lookup func(string) (string, bool), opts ...Option) error {
	v, ok := lookup(env)
	if !ok || v == "" {
		return RegisterUnconfigured(reg, svc, env)
	}
	dsn, err := ParseDSN(v)
	if err != nil {
		return fmt.Errorf("%s: %w", env, err)
	}
	return Register(reg, svc, dsn, opts...)
}

type invoke func(m *method, in, out proto.Message) func(context.Context) error

// serviceDesc builds the grpc.ServiceDesc of svc at run time, its unary
// handlers allocating the method's own message types.
func serviceDesc(svc protoreflect.ServiceDescriptor, run invoke) (*grpc.ServiceDesc, error) {
	client := Name(svc)
	desc := &grpc.ServiceDesc{
		ServiceName: string(svc.FullName()),
		// Any implementation satisfies an empty interface: the handlers
		// close over their state and never look at it.
		HandlerType: (*any)(nil),
		Metadata:    svc.ParentFile().Path(),
	}
	methods := svc.Methods()
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		if md.IsStreamingClient() || md.IsStreamingServer() {
			return nil, fmt.Errorf("egress: %s is streaming; an egress call is one request and one response", md.FullName())
		}
		call := wire.Call(md)
		if call == nil || call.GetMethod() == "" || call.GetPath() == "" {
			return nil, fmt.Errorf("egress: %s has no (w17.egress_call) — is this an egress client?", md.FullName())
		}
		m := &method{client: client, name: string(md.Name()), call: call}
		inType, outType := messageType(md.Input()), messageType(md.Output())
		fullName := "/" + string(svc.FullName()) + "/" + string(md.Name())
		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: string(md.Name()),
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := inType.New().Interface()
				if err := dec(in); err != nil {
					return nil, err
				}
				handle := func(ctx context.Context, req any) (any, error) {
					out := outType.New().Interface()
					if err := run(m, req.(proto.Message), out)(ctx); err != nil {
						return nil, err
					}
					return out, nil
				}
				if interceptor == nil {
					return handle(ctx, in)
				}
				return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: fullName}, handle)
			},
		})
	}
	return desc, nil
}

// messageType is md's linked Go type, or a dynamic one when the program has
// none (a descriptor built at run time).
func messageType(md protoreflect.MessageDescriptor) protoreflect.MessageType {
	if mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName()); err == nil && mt.Descriptor() == md {
		return mt
	}
	return dynamicpb.NewMessageType(md)
}
