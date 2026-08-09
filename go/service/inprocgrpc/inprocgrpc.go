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
// exactly what the transport would have delivered — and the handler's own
// outgoing side is rebuilt the way a real hop rebuilds it, so nothing the
// caller carried rides into a NESTED in-process call uninvited. See
// [serverContext] for what "observationally equivalent to a wire hop"
// means here and why it is not optional.
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
	"github.com/wandering-compiler/sdk/go/lib/principal"
)

var tracer = otel.Tracer("github.com/wandering-compiler/sdk/go/service/inprocgrpc")

// Conn is an in-process gRPC bridge — see the package doc. One Conn
// represents one backend tier; register that tier's servers on it (via
// the generated RegisterXxxServer calls), then build the callers'
// clients on it (via the generated NewXxxClient). Safe for concurrent
// use once registration is complete; register all servers before
// serving any caller (the typical main-goroutine wiring order).
type Conn struct {
	interceptor       grpc.UnaryServerInterceptor
	streamInterceptor grpc.StreamServerInterceptor
	methods           map[string]*methodEntry
	streams           map[string]*streamEntry
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

// WithStreamInterceptor sets the server-side STREAM interceptor run for
// every in-process streaming call on this Conn — the in-process equivalent
// of grpc.StreamInterceptor on a standalone bundle's *grpc.Server. One per
// Conn; compose several with [WithStreamInterceptors].
func WithStreamInterceptor(i grpc.StreamServerInterceptor) option {
	return func(c *Conn) { c.streamInterceptor = i }
}

// WithStreamInterceptors sets the server-side stream interceptor CHAIN run
// for every in-process streaming call — the in-process equivalent of
// grpc.ChainStreamInterceptor. Interceptors run outermost first (grpc-go
// chain semantics), so a tier can hand its slice straight through. An empty
// / nil slice installs nothing.
//
// This exists so the two deployments CAN agree. A composed binary runs a
// tier's servers on a Conn instead of a *grpc.Server, so a stream
// interceptor installed on the wire server has no counterpart here unless
// the composed wiring passes it — and a build stays green either way. No
// tier ships a stream chain today (the generated storage tier's chain is
// unary-only, and this transport mirrors the wire's recovery interceptor
// inline), so nothing wires this yet; whoever adds the first one must pass
// it here too, and this is the seam that makes that possible.
func WithStreamInterceptors(interceptors ...grpc.StreamServerInterceptor) option {
	return func(c *Conn) { c.streamInterceptor = chainStream(interceptors) }
}

// chainStream composes stream interceptors into one, outermost first
// (matching grpc-go's chainStreamInterceptors). Empty → nil.
func chainStream(interceptors []grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	n := len(interceptors)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return interceptors[0]
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		var build func(i int) grpc.StreamHandler
		build = func(i int) grpc.StreamHandler {
			if i == n {
				return handler
			}
			return func(srv any, ss grpc.ServerStream) error {
				return interceptors[i](srv, ss, info, build(i+1))
			}
		}
		return build(0)(srv, ss)
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
//
// CallOptions: [grpc.Header] and [grpc.Trailer] are honoured — the handler's
// grpc.SetHeader / grpc.SendHeader / grpc.SetTrailer land in them, as they
// would over the wire (see [inprocServerTransportStream]). Every other
// option is INERT by construction: they configure a wire call (wait-for-
// ready, compressor, max message size, per-RPC credentials, peer address)
// and there is no wire here. They are accepted and ignored rather than
// rejected, because a caller that passes one is not wrong — the composed
// deployment simply has nothing for it to configure.
func (c *Conn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) (err error) {
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

	ctx = serverContext(ctx)

	// A handler may set response header / trailer metadata through
	// grpc.SetHeader & friends, which need a ServerTransportStream in the
	// context — without one they FAIL (they don't silently no-op), so this
	// is what makes those calls work in-process at all. Delivery is
	// deferred so a handler that set headers before erroring still reports
	// them, matching the wire.
	sts := &inprocServerTransportStream{method: method}
	ctx = grpc.NewContextWithServerTransportStream(ctx, sts)
	defer sts.deliver(opts)

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

// inprocServerTransportStream is the unary call's stand-in for the
// transport stream grpc.SetHeader / grpc.SendHeader / grpc.SetTrailer
// operate on. Those helpers fetch it out of the handler's context and
// return an error when it is absent, so without one every header a
// re-hosted handler tried to set failed the call outright — while the same
// handler works over the wire.
//
// It collects what the handler set and hands it to the caller's
// [grpc.Header] / [grpc.Trailer] call options. The handler runs on the
// caller's goroutine for a unary call, but the mutex is kept because
// nothing stops a handler from setting a trailer from a goroutine it
// spawned, and an unsynchronised map write there would be a real race.
type inprocServerTransportStream struct {
	method string

	mu      sync.Mutex
	header  metadata.MD
	trailer metadata.MD
	sent    bool
}

func (s *inprocServerTransportStream) Method() string { return s.method }

// SetHeader accumulates header metadata until the headers are "sent".
// Setting after the send is an error, exactly as on the wire — the frame
// has left; pretending otherwise would hide a real ordering bug.
func (s *inprocServerTransportStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent {
		return status.Error(grpccodes.FailedPrecondition, "inprocgrpc: headers were already sent")
	}
	s.header = metadata.Join(s.header, md)
	return nil
}

func (s *inprocServerTransportStream) SendHeader(md metadata.MD) error {
	if err := s.SetHeader(md); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = true
	return nil
}

func (s *inprocServerTransportStream) SetTrailer(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trailer = metadata.Join(s.trailer, md)
	return nil
}

// deliver fills the caller's grpc.Header / grpc.Trailer call options.
// Options of any other kind configure a wire call and have nothing to
// configure here — see Invoke's doc.
func (s *inprocServerTransportStream) deliver(opts []grpc.CallOption) {
	if len(opts) == 0 {
		return
	}
	s.mu.Lock()
	header, trailer := s.header, s.trailer
	s.mu.Unlock()
	for _, o := range opts {
		switch v := o.(type) {
		case grpc.HeaderCallOption:
			if v.HeaderAddr != nil {
				*v.HeaderAddr = header
			}
		case grpc.TrailerCallOption:
			if v.TrailerAddr != nil {
				*v.TrailerAddr = trailer
			}
		}
	}
}

var _ grpc.ServerTransportStream = (*inprocServerTransportStream)(nil)

// serverContext rebuilds the caller's context into the one a handler would
// have been given across a REAL gRPC hop. The in-process shortcut must be
// observationally equivalent to the wire: a wire hop marshals the message
// (isolating it — copyProto does that here) and REBUILDS the metadata from
// the headers it received, and the shortcut must not quietly grant more
// than that rebuild would.
//
// Three steps, one per thing the wire does:
//
//  1. Relay the gateway's stamps. Every dialled inter-tier conn installs
//     principal.ForwardToOutgoing (core/grpcx.DialOpts) — a CLIENT-side
//     interceptor, so it does not exist on this path and has to be applied
//     here. It copies what the gateway wrote from its verified auth
//     response (`x-w17-user`, every `x-w17-scope-*`, every `w17-label-*`)
//     from the caller's incoming side onto its outgoing side, never
//     overwriting an explicit value, and moves nothing else.
//  2. Deliver: the transport hands the server the caller's OUTGOING
//     metadata as INCOMING, so handlers reading
//     metadata.FromIncomingContext (principal, language, paging, tx-id, …)
//     behave identically in-process. The flip is UNCONDITIONAL — when the
//     caller set no outgoing metadata the handler gets an empty incoming
//     side, which is what an empty header set delivers; skipping the flip
//     used to leave the CALLER's own incoming metadata visible instead.
//  3. Clear the outgoing side: a real server context is built from the
//     received headers and carries no outgoing metadata at all, so
//     everything a handler forwards downstream is explicit.
//
// Step 3 is the one that matters, and step 1 is what makes it safe. Without
// step 3 a handler reached in-process still carried the ORIGINAL caller's
// outgoing metadata, and any NESTED in-process call (the business facade
// calling storage, a plugin's typed-client adapter) re-flipped it — so
// `w17-tx-id` rode along and the nested handler ADOPTED the caller's
// distributed transaction in a composed binary while the identical code
// opened a fresh transaction standalone. That contract is not a matter of
// taste: lib/principal/forward_test.go pins tx routing OUT of the wire
// relay. Without step 1, clearing the outgoing side would have broken the
// principal inheritance composed tiers rely on (they would fail closed at
// storage's scope guard) — the propagation was accidental, and this makes
// it deliberate and identical to the wire's.
func serverContext(ctx context.Context) context.Context {
	ctx = principal.ForwardToOutgoing(ctx)
	md, _ := metadata.FromOutgoingContext(ctx)
	ctx = metadata.NewIncomingContext(ctx, md)
	return metadata.NewOutgoingContext(ctx, nil)
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
func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	e := c.streams[method]
	if e == nil {
		return nil, status.Errorf(grpccodes.Unimplemented, "inprocgrpc: no in-process handler registered for streaming %s", method)
	}

	// Rebuild the caller's context the way a wire hop would (as Invoke
	// does — see [serverContext]). The server context derives from the
	// caller's, so caller cancellation (or abandoning the stream) cancels
	// the handler's context too.
	serverBase := serverContext(ctx)
	spanCtx, span := tracer.Start(serverBase, method, trace.WithSpanKind(trace.SpanKindInternal))
	serverCtx, cancel := context.WithCancel(spanCtx)

	p := &inprocStream{
		clientCtx:   ctx,
		serverCtx:   serverCtx,
		cancel:      cancel,
		c2s:         make(chan proto.Message),
		s2c:         make(chan proto.Message),
		finished:    make(chan struct{}),
		headerReady: make(chan struct{}),
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
		// Run the tier's stream interceptor chain around the handler, as a
		// standalone bundle's grpc.ChainStreamInterceptor would. Without
		// this the two deployments could not agree even if the composed
		// wiring passed a chain — see [WithStreamInterceptors].
		err := c.dispatchStream(e, method, desc, p)
		if err != nil {
			span.SetStatus(otelcodes.Error, err.Error())
		}
		p.finish(err)
	}()

	return clientStream{p: p}, nil
}

// dispatchStream runs one streaming call's handler, wrapped in the Conn's
// stream interceptor chain when it has one. The StreamServerInfo mirrors
// what grpc-go builds from the same StreamDesc, so an interceptor can tell
// client- from server- from bidi-streaming; a caller that passes no desc
// (only low-level tests do) gets the method name and false flags rather
// than a panic.
func (c *Conn) dispatchStream(e *streamEntry, method string, desc *grpc.StreamDesc, p *inprocStream) error {
	if c.streamInterceptor == nil {
		return e.handler(e.impl, serverStream{p: p})
	}
	info := &grpc.StreamServerInfo{FullMethod: method}
	if desc != nil {
		info.IsClientStream = desc.ClientStreams
		info.IsServerStream = desc.ServerStreams
	}
	return c.streamInterceptor(e.impl, serverStream{p: p}, info, e.handler)
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
	s2c chan proto.Message // server → client (never closed; see finished)
	// finished is closed by finish() to end the stream.
	//
	// The end of a stream is signalled by a SEPARATE channel rather than by
	// closing s2c, because closing the channel a producer may still be
	// sending on is a panic — and on a handler-spawned goroutine no recover
	// can catch it, so in a composed binary one late Send takes the whole
	// PROCESS down where the wire would merely return an error. Reordering
	// close and cancel does not fix that: a sender parked in the select
	// picks randomly between a ready Done() and a closed send channel.
	finished chan struct{}

	closeSendOnce sync.Once
	finishOnce    sync.Once
	err           error // final status; readable once finished is observed closed

	// Response header / trailer metadata, the streaming twin of
	// inprocServerTransportStream: the handler sets it through its
	// grpc.ServerStream and the caller reads it through
	// ClientStream.Header / .Trailer. headerReady is closed once the
	// headers are "sent" — explicitly, on the first message, or when the
	// stream ends — so a caller parked in Header() is always released.
	// It is nil only on a bare inprocStream built by a low-level test.
	hdrMu       sync.Mutex
	header      metadata.MD
	trailer     metadata.MD
	headerOnce  sync.Once
	headerReady chan struct{}
}

// markHeaderSent releases anyone waiting in ClientStream.Header. Idempotent
// and safe on a bare stream (nil channel).
func (p *inprocStream) markHeaderSent() {
	p.headerOnce.Do(func() {
		if p.headerReady != nil {
			close(p.headerReady)
		}
	})
}

// headerSent reports whether the response headers have been flushed. The
// closed channel is the happens-before edge, so no lock is needed.
func (p *inprocStream) headerSent() bool {
	if p.headerReady == nil {
		return false
	}
	select {
	case <-p.headerReady:
		return true
	default:
		return false
	}
}

// finish records the handler's final error and closes the server→client
// channel, ending the caller's RecvMsg loop (io.EOF on nil, else err). The
// handler goroutine calls it exactly once, after it stops sending on s2c.
func (p *inprocStream) finish(err error) {
	p.finishOnce.Do(func() {
		p.err = err
		// A stream that ends without ever sending headers still answers
		// Header() — the wire delivers a trailers-only response there, and
		// a Header() that could block forever would be worse than one that
		// returns empty.
		p.markHeaderSent()
		close(p.finished)
		p.cancel()
	})
}

// clientStream is the caller's grpc.ClientStream view.
type clientStream struct{ p *inprocStream }

func (s clientStream) Context() context.Context { return s.p.clientCtx }

// Header blocks until the handler sends its response headers (explicitly,
// with its first message, or by ending the stream) and returns them —
// the wire contract. A cancelled caller context releases it with that
// error rather than waiting for a handler that may never answer.
func (s clientStream) Header() (metadata.MD, error) {
	if s.p.headerReady != nil {
		select {
		case <-s.p.clientCtx.Done():
			return nil, s.p.clientCtx.Err()
		case <-s.p.headerReady:
		}
	}
	s.p.hdrMu.Lock()
	defer s.p.hdrMu.Unlock()
	return s.p.header, nil
}

// Trailer returns the handler's trailer metadata. Per the gRPC contract it
// is only populated once the stream has ended (RecvMsg returned an error).
func (s clientStream) Trailer() metadata.MD {
	s.p.hdrMu.Lock()
	defer s.p.hdrMu.Unlock()
	return s.p.trailer
}

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
	case <-s.p.finished:
		// No send can still be pending: finish runs after the handler
		// returned, and any sender parked on s2c is released by this same
		// channel with an error.
		if s.p.err != nil {
			return s.p.err
		}
		return io.EOF
	case v := <-s.p.s2c:
		return copyProto(m, v)
	}
}

// serverStream is the handler's grpc.ServerStream view. Header/trailer
// metadata is a no-op in-process (the caller reads neither — there is no
// wire frame to carry them); the identity/language metadata handlers care
// about is already threaded in as incoming context.
type serverStream struct{ p *inprocStream }

func (s serverStream) Context() context.Context { return s.p.serverCtx }

// SetHeader accumulates response headers until they are sent; sending is
// what releases a caller waiting in ClientStream.Header.
func (s serverStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	s.p.hdrMu.Lock()
	defer s.p.hdrMu.Unlock()
	if s.p.headerSent() {
		return status.Error(grpccodes.FailedPrecondition, "inprocgrpc: headers were already sent")
	}
	s.p.header = metadata.Join(s.p.header, md)
	return nil
}

func (s serverStream) SendHeader(md metadata.MD) error {
	if err := s.SetHeader(md); err != nil {
		return err
	}
	s.p.markHeaderSent()
	return nil
}

func (s serverStream) SetTrailer(md metadata.MD) {
	if md.Len() == 0 {
		return
	}
	s.p.hdrMu.Lock()
	defer s.p.hdrMu.Unlock()
	s.p.trailer = metadata.Join(s.p.trailer, md)
}

func (s serverStream) SendMsg(m any) error {
	cp, err := cloneMsg(m)
	if err != nil {
		return err
	}
	// The wire flushes response headers with the first message; do the same
	// so a caller waiting in Header() is released by the first frame.
	s.p.markHeaderSent()
	select {
	case <-s.p.serverCtx.Done():
		return s.p.serverCtx.Err()
	case <-s.p.finished:
		// A goroutine the handler spawned, still sending after the handler
		// returned. The wire answers this with an error; so do we.
		return status.Error(grpccodes.Canceled, "inprocgrpc: SendMsg after the handler returned")
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
