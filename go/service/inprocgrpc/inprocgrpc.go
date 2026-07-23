// Package inprocgrpc is an in-process gRPC bridge: it lets a single
// binary host several generated gRPC tiers (storage, business, …) and
// have one tier call another DIRECTLY — no socket, no wire
// (de)serialization — while the generated client + server code stays
// byte-for-byte the same as the networked deployment.
//
// It works because the generated code is written against interfaces:
//
//   - generated `RegisterXxxServer(s grpc.ServiceRegistrar, impl)`
//     registers a server impl. [Conn] implements grpc.ServiceRegistrar,
//     so the SAME register call collects the impl here.
//   - generated `NewXxxClient(cc grpc.ClientConnInterface)` builds a
//     client. [Conn] implements grpc.ClientConnInterface, so a client
//     built on it dispatches each call straight to the registered
//     server handler.
//
// So a composed binary builds one Conn per backend tier, registers that
// tier's servers on it, and hands the Conn to the callers' generated
// clients (the gateway's per-service clients, the business tier's
// injected ClientSet, the admin handlers' client). Tier-to-tier calls
// then run as plain in-process function calls.
//
// Tracing stays functional: every call opens an OTel span named after
// the gRPC method, so a composed binary's traces show the same per-hop
// breakdown the networked deployment would (one span per call instead
// of the wire transport's client+server span pair — the same process,
// so one span is correct). Request metadata the caller set as OUTGOING
// (the wire convention — `x-w17-user`, `w17-language`, …) is flipped to
// INCOMING so server handlers reading metadata.FromIncomingContext see
// exactly what the transport would have delivered.
package inprocgrpc

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sync"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/core/observx"
)

var tracer = otel.Tracer("github.com/wandering-compiler/sdk/go/service/inprocgrpc")

// Conn is an in-process gRPC bridge — see the package doc. One Conn
// represents one backend tier; register that tier's servers on it (via
// the generated RegisterXxxServer calls), then build the callers'
// clients on it (via the generated NewXxxClient). Safe for concurrent
// use once registration is complete; register all servers before
// serving any caller (the typical main-goroutine wiring order).
type Conn struct {
	interceptor grpc.UnaryServerInterceptor
	methods     map[string]*methodEntry
	streams     map[string]*streamEntry
}

// methodEntry binds one fully-qualified method to its server impl + the
// generated unary handler (the same func stored in
// grpc.MethodDesc.Handler).
type methodEntry struct {
	impl    any
	handler func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error)
}

// streamEntry binds one fully-qualified streaming method to its server
// impl + the generated stream handler (the same func stored in
// grpc.StreamDesc.Handler).
type streamEntry struct {
	impl    any
	handler grpc.StreamHandler
}

// option configures a [Conn].
type option func(*Conn)

// WithUnaryInterceptor sets the server-side unary interceptor run for
// every in-process call on this Conn — the in-process equivalent of the
// interceptors a standalone bundle installs on its *grpc.Server (e.g.
// the storage tier's auto-rollback + eventbus-emit interceptors). One
// per Conn; compose multiple into one with a chaining interceptor.
func WithUnaryInterceptor(i grpc.UnaryServerInterceptor) option {
	return func(c *Conn) { c.interceptor = i }
}

// WithUnaryInterceptors sets the server-side interceptor CHAIN run for
// every in-process call — the in-process equivalent of a bundle's
// grpc.ChainUnaryInterceptor server option. Interceptors run outermost
// first (grpc-go chain semantics), so a tier can hand its
// Resources.Interceptors() slice straight through. An empty / nil slice
// installs no interceptor. A composed -server uses this to apply the
// storage tier's auto-rollback + eventbus-emit chain on its Conn.
func WithUnaryInterceptors(interceptors ...grpc.UnaryServerInterceptor) option {
	return func(c *Conn) { c.interceptor = chainUnary(interceptors) }
}

// chainUnary composes interceptors into a single one, outermost first
// (matching grpc-go's chainUnaryInterceptors). Empty → nil.
func chainUnary(interceptors []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	n := len(interceptors)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return interceptors[0]
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		var build func(i int) grpc.UnaryHandler
		build = func(i int) grpc.UnaryHandler {
			if i == n {
				return handler
			}
			return func(ctx context.Context, req any) (any, error) {
				return interceptors[i](ctx, req, info, build(i+1))
			}
		}
		return build(0)(ctx, req)
	}
}

// New returns an empty Conn ready for RegisterService calls.
func New(opts ...option) *Conn {
	c := &Conn{methods: map[string]*methodEntry{}, streams: map[string]*streamEntry{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// RegisterService implements grpc.ServiceRegistrar: the generated
// RegisterXxxServer(conn, impl) calls land here, collecting each method's
// handler keyed by its fully-qualified name — unary methods (sd.Methods,
// dispatched by Invoke) and streaming methods (sd.Streams, dispatched by
// NewStream over an in-memory channel pair). Streaming lets a composed
// binary fold a tier that exposes a server-/client-/bidi-streaming method
// (e.g. the re-hosted CodegenService's GenerateProject) the same way the
// networked deployment would.
func (c *Conn) RegisterService(sd *grpc.ServiceDesc, impl any) {
	for i := range sd.Methods {
		m := sd.Methods[i]
		full := "/" + sd.ServiceName + "/" + m.MethodName
		c.methods[full] = &methodEntry{impl: impl, handler: m.Handler}
	}
	for i := range sd.Streams {
		s := sd.Streams[i]
		full := "/" + sd.ServiceName + "/" + s.StreamName
		c.streams[full] = &streamEntry{impl: impl, handler: s.Handler}
	}
}

// Invoke implements grpc.ClientConnInterface. It dispatches the call to
// the registered server handler in-process: no marshalling, no socket.
// args / reply are the caller's already-typed request / response
// messages; the handler receives a shallow proto-copy of args (the
// grpc handler contract allocates its own request value) and its result
// is copied back into reply. Wrapped in an OTel span + metadata flip
// (see the package doc).
func (c *Conn) Invoke(ctx context.Context, method string, args, reply any, _ ...grpc.CallOption) (err error) {
	e := c.methods[method]
	if e == nil {
		return status.Errorf(grpccodes.Unimplemented, "inprocgrpc: no in-process handler registered for %s", method)
	}

	ctx, span := tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	// A re-hosted unary handler runs in-process on the caller's
	// goroutine, so an uncaught panic would crash the whole composed
	// binary. Mirror the wire server's recover net (and NewStream's):
	// route value+stack through observx, return a GENERIC Internal so
	// the panic value never reaches the client. Reuses the same helper
	// generated handlers emit.
	defer grpcerr.RecoverPanic(ctx, method, &err)

	// The wire transport delivers the caller's OUTGOING metadata to the
	// server as INCOMING metadata. Reproduce that so handlers reading
	// metadata.FromIncomingContext (auth principal, language, tx-id, …)
	// behave identically in-process.
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		ctx = metadata.NewIncomingContext(ctx, md)
	}

	dec := func(target any) error {
		return copyProto(target, args)
	}
	resp, err := e.handler(e.impl, ctx, dec, c.interceptor)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
		return err
	}
	if err := copyProto(reply, resp); err != nil {
		return err
	}
	return nil
}

// NewStream implements grpc.ClientConnInterface for streaming methods. It
// runs the registered server stream handler in a goroutine and bridges it
// to the caller over an in-memory channel pair — no socket, no wire
// (de)serialization — so a composed binary's tier-to-tier streaming call
// runs as a direct in-process pipe. Works for server-, client-, and
// bidi-streaming (the generated client/server code is identical to the
// networked deployment; only the transport differs).
//
// The channels are UNBUFFERED: each server SendMsg blocks until the caller
// RecvMsgs it, giving natural backpressure (the producing handler can't
// outrun the consumer — e.g. the gateway forwarding to a real wire client).
func (c *Conn) NewStream(ctx context.Context, _ *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	e := c.streams[method]
	if e == nil {
		return nil, status.Errorf(grpccodes.Unimplemented, "inprocgrpc: no in-process handler registered for streaming %s", method)
	}

	// The wire transport delivers the caller's OUTGOING metadata to the
	// server as INCOMING metadata — reproduce that (as Invoke does). The
	// server context derives from the caller's, so caller cancellation
	// (or abandoning the stream) cancels the handler's context too.
	serverBase := ctx
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		serverBase = metadata.NewIncomingContext(ctx, md)
	}
	spanCtx, span := tracer.Start(serverBase, method, trace.WithSpanKind(trace.SpanKindInternal))
	serverCtx, cancel := context.WithCancel(spanCtx)

	p := &inprocStream{
		clientCtx: ctx,
		serverCtx: serverCtx,
		cancel:    cancel,
		c2s:       make(chan proto.Message),
		s2c:       make(chan proto.Message),
	}

	go func() {
		defer span.End()
		defer func() {
			if r := recover(); r != nil {
				// Route value+stack through observx (Sentry + the OTel
				// span) so the panic is visible in telemetry, then finish
				// the stream with a GENERIC Internal — the panic value
				// never reaches the client (in a single-binary deploy the
				// gateway relays this status to external callers). Mirrors
				// grpcerr.RecoverStreamInterceptor; inline because we
				// report completion via p.finish, not a return value.
				observx.ReportError(serverCtx, fmt.Errorf("PANIC %s: %v\n%s", method, r, debug.Stack()))
				p.finish(status.Errorf(grpccodes.Internal, "%s: internal error", method))
			}
		}()
		err := e.handler(e.impl, serverStream{p: p})
		if err != nil {
			span.SetStatus(otelcodes.Error, err.Error())
		}
		p.finish(err)
	}()

	return clientStream{p: p}, nil
}

// inprocStream is the shared core of one in-process streaming call: two
// unbuffered channels (client→server, server→client) plus completion
// state. The clientStream and serverStream views read/write opposite ends.
// Each channel has a single producer (the caller goroutine writes c2s; the
// handler goroutine writes s2c) and the producer is the only closer, so
// there is no send-on-closed-channel race.
type inprocStream struct {
	clientCtx context.Context
	serverCtx context.Context
	cancel    context.CancelFunc

	c2s chan proto.Message // client → server (CloseSend closes it)
	s2c chan proto.Message // server → client (finish closes it)

	closeSendOnce sync.Once
	finishOnce    sync.Once
	err           error // final status; readable once s2c is observed closed
}

// finish records the handler's final error and closes the server→client
// channel, ending the caller's RecvMsg loop (io.EOF on nil, else err). The
// handler goroutine calls it exactly once, after it stops sending on s2c.
func (p *inprocStream) finish(err error) {
	p.finishOnce.Do(func() {
		p.err = err
		close(p.s2c)
		p.cancel()
	})
}

// clientStream is the caller's grpc.ClientStream view.
type clientStream struct{ p *inprocStream }

func (s clientStream) Context() context.Context     { return s.p.clientCtx }
func (s clientStream) Header() (metadata.MD, error) { return nil, nil }
func (s clientStream) Trailer() metadata.MD         { return nil }

func (s clientStream) CloseSend() error {
	s.p.closeSendOnce.Do(func() { close(s.p.c2s) })
	return nil
}

func (s clientStream) SendMsg(m any) error {
	cp, err := cloneMsg(m)
	if err != nil {
		return err
	}
	// Watch the server side too. serverCtx is cancelled by finish() when the
	// handler returns, but it is a CHILD of clientCtx — cancelling it does NOT
	// cancel clientCtx. So a caller that keeps sending after the handler
	// returned early (an errored client-streaming/bidi handler that stopped
	// draining c2s) would otherwise block forever on the unbuffered c2s send
	// whose only reader — the handler goroutine — is already gone. The
	// serverCtx.Done() arm unblocks the send with the server's context error,
	// mirroring the wire transport (SendMsg after the server ends the stream
	// returns an error rather than hanging); the caller then reads the real
	// status via RecvMsg. serverCtx may be nil in low-level unit tests that
	// drive a bare inprocStream; a nil Done channel is simply never ready.
	var serverDone <-chan struct{}
	if s.p.serverCtx != nil {
		serverDone = s.p.serverCtx.Done()
	}
	select {
	case <-s.p.clientCtx.Done():
		return s.p.clientCtx.Err()
	case <-serverDone:
		return s.p.serverCtx.Err()
	case s.p.c2s <- cp:
		return nil
	}
}

func (s clientStream) RecvMsg(m any) error {
	select {
	case <-s.p.clientCtx.Done():
		return s.p.clientCtx.Err()
	case v, ok := <-s.p.s2c:
		if !ok {
			if s.p.err != nil {
				return s.p.err
			}
			return io.EOF
		}
		return copyProto(m, v)
	}
}

// serverStream is the handler's grpc.ServerStream view. Header/trailer
// metadata is a no-op in-process (the caller reads neither — there is no
// wire frame to carry them); the identity/language metadata handlers care
// about is already threaded in as incoming context.
type serverStream struct{ p *inprocStream }

func (s serverStream) Context() context.Context     { return s.p.serverCtx }
func (s serverStream) SetHeader(metadata.MD) error  { return nil }
func (s serverStream) SendHeader(metadata.MD) error { return nil }
func (s serverStream) SetTrailer(metadata.MD)       {}

func (s serverStream) SendMsg(m any) error {
	cp, err := cloneMsg(m)
	if err != nil {
		return err
	}
	select {
	case <-s.p.serverCtx.Done():
		return s.p.serverCtx.Err()
	case s.p.s2c <- cp:
		return nil
	}
}

func (s serverStream) RecvMsg(m any) error {
	select {
	case <-s.p.serverCtx.Done():
		return s.p.serverCtx.Err()
	case v, ok := <-s.p.c2s:
		if !ok {
			return io.EOF // client called CloseSend
		}
		return copyProto(m, v)
	}
}

// cloneMsg deep-copies a stream message at SEND time so the sender may
// reuse its value the moment SendMsg returns (the grpc contract), mirroring
// Invoke's copy-in/copy-out for unary. The receiver lands it via copyProto.
func cloneMsg(m any) (proto.Message, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("inprocgrpc: stream message %T is not a proto.Message", m)
	}
	return proto.Clone(pm), nil
}

// copyProto deep-copies src into dst (both proto messages). Used to
// honour the grpc handler contract (the handler owns its request value)
// and to land the response in the caller's reply value — a struct copy,
// NOT a wire (de)serialization round-trip.
func copyProto(dst, src any) error {
	dm, ok := dst.(proto.Message)
	if !ok {
		return fmt.Errorf("inprocgrpc: destination %T is not a proto.Message", dst)
	}
	sm, ok := src.(proto.Message)
	if !ok {
		return fmt.Errorf("inprocgrpc: source %T is not a proto.Message", src)
	}
	proto.Reset(dm)
	proto.Merge(dm, sm)
	return nil
}

// Compile-time assertions: Conn is both a server registrar and a client
// conn — the two halves of the bridge.
var (
	_ grpc.ServiceRegistrar    = (*Conn)(nil)
	_ grpc.ClientConnInterface = (*Conn)(nil)
)
