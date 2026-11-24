// Package mail is the runtime of a mail client: the egress client
// `w17ctl client generate --mail` declares, whose service has one method,
//
//	rpc Send(w17.MailMessage) returns (w17.MailReceipt);
//
// served in-process like every egress client (sdk/go/lib/egress routes a
// file marked `(w17.egress_client) = { mail: true }` here) and configured by
// its W17_CLIENT_<NAME>_DSN: an SMTP server or a transactional-mail API (see
// [ParseDSN] and w17/mail.proto).
//
// What a call does: the message is checked (addresses, a sender, a body, a
// size within [MaxMessageBytes], no header that spans lines), composed as
// MIME — text and HTML as alternatives, inline parts as related, files as
// mixed, Bcc never written — and handed to the provider. A failure the
// provider certainly did not accept is retried with a capped back-off: an
// SMTP 4xx, a connection that never got as far as the end of the message,
// HTTP 429 or 503, or (Resend, which takes an idempotency key) any 5xx. Any
// other failure ends the call: the platform never risks a second copy of a
// message, and an unknown outcome says so (OUTCOME_UNKNOWN). Every attempt is
// in one client span; no address, subject or credential is written to it or
// to an error.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// Reasons a mail error carries in its ErrorInfo (domain = the client),
// beside UPSTREAM_SMTP_<code> and UPSTREAM_HTTP_<status>.
const (
	ReasonUnconfigured = "CLIENT_UNCONFIGURED"
	ReasonInvalid      = "MESSAGE_INVALID"
	ReasonTransport    = "UPSTREAM_TRANSPORT"
	// The message may have been accepted: the connection ended before the
	// provider's answer. Not retried.
	ReasonOutcomeUnknown = "OUTCOME_UNKNOWN"
	// An smtp:// server without STARTTLS.
	ReasonTLSUnavailable = "TLS_UNAVAILABLE"
)

const maxErrorBody = 4 << 10

// Option configures [Register].
type Option func(*options)

type options struct {
	transport http.RoundTripper
	tls       *tls.Config
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
}

// WithTransport sends an API provider's requests through rt.
func WithTransport(rt http.RoundTripper) Option { return func(o *options) { o.transport = rt } }

// WithTLSConfig is the TLS configuration of an SMTP connection (its roots,
// for a test server); ServerName defaults to the DSN's host.
func WithTLSConfig(c *tls.Config) Option { return func(o *options) { o.tls = c } }

// sender is one provider. send is one attempt; its outcome says whether
// the message went, and if not, whether trying again is safe.
type sender interface {
	send(ctx context.Context, m *message, raw []byte) (providerID string, o outcome)
	// wantsMIME: the provider takes the composed message (SMTP, Mailgun's
	// MIME endpoint), so its Message-ID is ours.
	wantsMIME() bool
}

type outcome struct {
	err       error
	retryable bool
	wait      time.Duration // the provider's Retry-After, when it gave one
}

func sent() outcome                             { return outcome{} }
func notSent(err error) outcome                 { return outcome{err: err, retryable: true} }
func final(err error) outcome                   { return outcome{err: err} }
func (o outcome) after(d time.Duration) outcome { o.wait = d; return o }

// Register serves svc, a mail client's service, sending through the
// provider dsn names. Nothing is contacted here.
func Register(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, client string, dsn *DSN, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	s, err := newSender(client, dsn, o)
	if err != nil {
		return fmt.Errorf("mail client %s: %w", client, err)
	}
	c := &caller{client: client, dsn: dsn, sender: s}
	return serve(reg, svc, client, c.send)
}

// RegisterUnconfigured serves svc refusing every call with
// FAILED_PRECONDITION naming env.
func RegisterUnconfigured(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, client, env string) error {
	return serve(reg, svc, client, func(context.Context, *w17pb.MailMessage) (*w17pb.MailReceipt, error) {
		return nil, callError(codes.FailedPrecondition, client, ReasonUnconfigured,
			"the client is not configured in this bundle: set "+env, map[string]string{"env": env})
	})
}

// RegisterFromEnv is [Register] configured from env read through lookup,
// or [RegisterUnconfigured] when it is unset. A DSN that does not parse is
// a boot error naming the variable, never its value.
func RegisterFromEnv(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, client, env string, lookup func(string) (string, bool), opts ...Option) error {
	v, ok := lookup(env)
	if !ok || v == "" {
		return RegisterUnconfigured(reg, svc, client, env)
	}
	dsn, err := ParseDSN(v)
	if err != nil {
		return fmt.Errorf("%s: %w", env, err)
	}
	return Register(reg, svc, client, dsn, opts...)
}

func newSender(client string, dsn *DSN, o options) (sender, error) {
	switch dsn.Provider {
	case SMTP, SMTPS:
		dial := o.dial
		if dial == nil {
			d := &net.Dialer{Timeout: 10 * time.Second}
			dial = d.DialContext
		}
		return &smtpSender{client: client, dsn: dsn, tls: o.tls, dial: dial}, nil
	case Postmark, SendGrid, Mailgun, Resend:
		rt := o.transport
		if rt == nil {
			rt = http.DefaultTransport
		}
		return &apiSender{client: client, dsn: dsn, http: &http.Client{Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", dsn.Provider)
}

// serve registers svc's one method, Send, on reg.
func serve(reg grpc.ServiceRegistrar, svc protoreflect.ServiceDescriptor, client string, send func(context.Context, *w17pb.MailMessage) (*w17pb.MailReceipt, error)) error {
	if err := CheckService(svc); err != nil {
		return fmt.Errorf("mail client %s: %w", client, err)
	}
	fullName := "/" + string(svc.FullName()) + "/Send"
	reg.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(svc.FullName()),
		HandlerType: (*any)(nil),
		Metadata:    svc.ParentFile().Path(),
		Methods: []grpc.MethodDesc{{
			MethodName: "Send",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := &w17pb.MailMessage{}
				if err := dec(in); err != nil {
					return nil, err
				}
				handle := func(ctx context.Context, req any) (any, error) { return send(ctx, req.(*w17pb.MailMessage)) }
				if interceptor == nil {
					return handle(ctx, in)
				}
				return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: fullName}, handle)
			},
		}},
	}, struct{}{})
	return nil
}

// CheckService refuses a service that is not a mail client's: exactly one
// method, `Send(w17.MailMessage) returns (w17.MailReceipt)`, unary.
func CheckService(svc protoreflect.ServiceDescriptor) error {
	ms := svc.Methods()
	if ms.Len() != 1 || ms.Get(0).Name() != "Send" {
		return fmt.Errorf("%s: a mail client's service has one method, Send", svc.FullName())
	}
	md := ms.Get(0)
	in, out := (&w17pb.MailMessage{}).ProtoReflect().Descriptor().FullName(), (&w17pb.MailReceipt{}).ProtoReflect().Descriptor().FullName()
	if md.IsStreamingClient() || md.IsStreamingServer() || md.Input().FullName() != in || md.Output().FullName() != out {
		return fmt.Errorf("%s: want rpc Send(%s) returns (%s)", md.FullName(), in, out)
	}
	return nil
}

// sleep waits d or until ctx is done; a variable so tests need not wait.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	backoffBase = 500 * time.Millisecond
	backoffCap  = 10 * time.Second
)

type caller struct {
	client string
	dsn    *DSN
	sender sender
}

func (c *caller) send(ctx context.Context, in *w17pb.MailMessage) (*w17pb.MailReceipt, error) {
	m, err := prepare(in, c.dsn.From)
	if err != nil {
		return nil, callError(codes.InvalidArgument, c.client, ReasonInvalid, err.Error(), nil)
	}
	ctx, span := observx.Tracer().Start(ctx, c.client+" send",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("peer.service", c.client),
			attribute.String("w17.mail.provider", string(c.dsn.Provider)),
			attribute.String("server.address", c.dsn.Server()),
			attribute.Int("w17.mail.recipients", len(m.recipients())),
			attribute.Int("w17.mail.attachments", len(in.GetAttachments())),
		))
	defer span.End()
	r, err := c.attempts(ctx, span, m)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return r, err
}

func (c *caller) attempts(ctx context.Context, span trace.Span, m *message) (*w17pb.MailReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()
	var raw []byte
	if c.sender.wantsMIME() {
		raw = compose(m)
	}
	for attempt := 1; ; attempt++ {
		actx, acancel := context.WithTimeout(ctx, c.dsn.timeout())
		id, o := c.sender.send(actx, m, raw)
		acancel()
		span.SetAttributes(attribute.Int("w17.mail.attempts", attempt))
		if o.err == nil {
			r := &w17pb.MailReceipt{ProviderId: id, Attempts: int32(attempt)} //nolint:gosec // G115: bounded by max_attempts
			if c.sender.wantsMIME() {
				r.MessageId = m.messageID
			}
			return r, nil
		}
		if ctx.Err() != nil {
			return nil, c.ended(ctx, attempt, o.err)
		}
		if !o.retryable || attempt >= c.dsn.maxAttempts() {
			return nil, withAttempts(o.err, attempt)
		}
		d := backoff(attempt)
		if o.wait > 0 {
			d = min(o.wait, backoffCap)
		}
		span.AddEvent("mail.retry", trace.WithAttributes(
			attribute.Int("attempt", attempt),
			attribute.String("cause", status.Convert(o.err).Message()),
			attribute.String("backoff", d.String()),
		))
		if sleep(ctx, d) != nil {
			return nil, c.ended(ctx, attempt, o.err)
		}
	}
}

func (c *caller) ended(ctx context.Context, attempts int, last error) error {
	code := codes.DeadlineExceeded
	if errors.Is(ctx.Err(), context.Canceled) {
		code = codes.Canceled
	}
	meta := map[string]string{"attempts": strconv.Itoa(attempts)}
	if last != nil {
		meta["last_failure"] = status.Convert(last).Message()
	}
	return callError(code, c.client, ReasonTransport, "call ended: "+ctx.Err().Error(), meta)
}

func backoff(attempt int) time.Duration {
	d := backoffBase << (attempt - 1)
	if d <= 0 || d > backoffCap {
		return backoffCap
	}
	return d
}

// callError is a mail error: a status with a google.rpc.ErrorInfo whose
// domain is the client. Neither the message nor the metadata carries the
// DSN or an address.
func callError(code codes.Code, client, reason, msg string, meta map[string]string) error {
	st := status.New(code, client+": "+msg)
	if withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Domain: client, Reason: reason, Metadata: meta}); err == nil {
		st = withInfo
	}
	return st.Err()
}

// withAttempts records how many attempts the call made on err's ErrorInfo.
func withAttempts(err error, attempts int) error {
	st := status.Convert(err)
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = proto.Clone(i).(*errdetails.ErrorInfo)
		}
	}
	if info == nil {
		return err
	}
	if info.Metadata == nil {
		info.Metadata = map[string]string{}
	}
	info.Metadata["attempts"] = strconv.Itoa(attempts)
	out, derr := status.New(st.Code(), st.Message()).WithDetails(info)
	if derr != nil {
		return err
	}
	return out.Err()
}

func transportFailure(client, msg string) error {
	return callError(codes.Unavailable, client, ReasonTransport, "provider unreachable: "+msg, nil)
}

// transportText is a transport error without what could carry a secret: a
// *url.Error's URL (an API key in a query) is dropped.
func transportText(err error) string {
	var u *url.Error
	for errors.As(err, &u) {
		err = u.Err
	}
	return err.Error()
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return strings.ToValidUTF8(string(b), "?")
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return strings.ToValidUTF8(string(b[:cut]), "?") + "…"
}
