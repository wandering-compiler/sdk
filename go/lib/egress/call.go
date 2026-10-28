package egress

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/egress/wire"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The call policy's defaults. A DSN overrides the attempt timeout and the
// attempt count; the rest is fixed.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultMaxAttempts = 3
	// TotalTimeout bounds the whole call, every attempt and back-off
	// included: counting attempts alone still allows an unbounded stall.
	TotalTimeout = 30 * time.Second
	// MaxResponseBytes caps a response body, enforced while reading.
	MaxResponseBytes = 10 << 20

	backoffBase = 200 * time.Millisecond
	// backoffCap caps one back-off, Retry-After included: an uncapped
	// exponential sleeps for hours by attempt fifteen.
	backoffCap = 5 * time.Second
)

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

// method is one egress call of a client: what its options say about the
// HTTP request, read once at registration.
type method struct {
	client string
	name   string // the proto method name, for hooks
	call   *w17pb.EgressCall
}

// caller makes the calls of one client.
type caller struct {
	client string
	dsn    *DSN
	http   *http.Client
}

func newCaller(client string, dsn *DSN, rt http.RoundTripper) *caller {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// The proxy is the DSN's or none: the environment's HTTP_PROXY is
		// not this client's configuration.
		t.Proxy = nil
		if dsn.Proxy != nil {
			t.Proxy = http.ProxyURL(dsn.Proxy)
		}
		rt = t
	}
	return &caller{client: client, dsn: dsn, http: &http.Client{
		Transport: rt,
		// A redirect is the upstream's answer, not a second request this
		// client decided to make — with the credential — elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *caller) timeout() time.Duration {
	if c.dsn.Timeout > 0 {
		return c.dsn.Timeout
	}
	return DefaultTimeout
}

func (c *caller) maxAttempts() int {
	if c.dsn.MaxAttempts > 0 {
		return c.dsn.MaxAttempts
	}
	return DefaultMaxAttempts
}

// do makes one call: in is the request message, out receives the decoded
// success body.
func (c *caller) do(ctx context.Context, m *method, in, out proto.Message) error {
	ctx, span := observx.Tracer().Start(ctx, c.client+" "+m.call.GetMethod()+" "+m.call.GetPath(),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("peer.service", c.client),
			attribute.String("http.request.method", m.call.GetMethod()),
			attribute.String("url.template", m.call.GetPath()),
			attribute.String("server.address", c.dsn.Base.Host),
		))
	defer span.End()
	err := c.attempts(ctx, span, m, in, out)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return err
}

func (c *caller) attempts(ctx context.Context, span trace.Span, m *method, in, out proto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()

	req, err := wire.Split(in)
	if err != nil {
		return callError(codes.InvalidArgument, c.client, ReasonBadRequest, err.Error(), nil)
	}
	target, err := c.target(m.call, req)
	if err != nil {
		return callError(codes.InvalidArgument, c.client, ReasonBadRequest, err.Error(), nil)
	}
	body, contentType, err := encodeBody(m.call, req)
	if err != nil {
		return callError(codes.InvalidArgument, c.client, ReasonBadRequest, err.Error(), nil)
	}

	retryable := idempotent(m.call.GetMethod())
	if h := m.call.GetIdempotencyHeader(); h != "" {
		// One key per call, every attempt reusing it, unless the caller
		// chose one.
		if req.Header.Get(h) == "" {
			req.Header.Set(h, newKey())
		}
		retryable = true
	}
	max := 1
	if retryable {
		max = c.maxAttempts()
	}

	for attempt := 1; ; attempt++ {
		status, respBody, wait, err := c.attempt(ctx, m, target, req.Header, body, contentType, out)
		span.SetAttributes(attribute.Int("w17.egress.attempts", attempt))
		if err == nil && status/100 == 2 {
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			return nil
		}
		var final error
		cause := ""
		switch {
		case err != nil && errors.Is(err, errStop):
			return err.(stopError).err
		case err != nil && ctx.Err() != nil:
			return c.ended(ctx, attempt, "")
		case err != nil:
			msg := transportText(err)
			cause = "transport: " + msg
			final = callError(codes.Unavailable, c.client, ReasonTransport, "upstream unreachable: "+msg,
				map[string]string{"attempts": strconv.Itoa(attempt)})
		default:
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			cause = "HTTP " + strconv.Itoa(status)
			final = httpError(c.client, status, attempt, respBody)
			if !retryStatus(status) {
				return final
			}
		}
		if attempt >= max {
			return final
		}
		d := backoff(attempt)
		if wait > 0 {
			d = min(wait, backoffCap)
		}
		// Every retry is visible: a swallowed one makes a wedged client
		// look like an idle healthy one.
		span.AddEvent("egress.retry", trace.WithAttributes(
			attribute.Int("attempt", attempt),
			attribute.String("cause", cause),
			attribute.String("backoff", d.String()),
		))
		if sleep(ctx, d) != nil {
			return c.ended(ctx, attempt, cause)
		}
	}
}

// ended is the error of a call whose context ran out — the caller's
// deadline or the call's total one, or the caller went away: that is the
// answer, not the upstream. last is the failure the call was waiting to
// retry, if any.
func (c *caller) ended(ctx context.Context, attempts int, last string) error {
	code := codes.DeadlineExceeded
	if errors.Is(ctx.Err(), context.Canceled) {
		code = codes.Canceled
	}
	meta := map[string]string{"attempts": strconv.Itoa(attempts)}
	if last != "" {
		meta["last_failure"] = last
	}
	return callError(code, c.client, ReasonTransport, "call ended: "+ctx.Err().Error(), meta)
}

// errStop marks an error that ends the call as it is, without a retry: a
// hook's refusal, a body over the cap, an undecodable success.
var errStop = errors.New("stop")

type stopError struct{ err error }

func (stopError) Error() string   { return "stop" }
func (stopError) Is(t error) bool { return t == errStop }

func stop(err error) error { return stopError{err} }

// attempt sends the request once. It returns the status and, for a failure,
// the (capped) body; for a success it decodes into out. wait is the
// upstream's Retry-After, zero when it sent none.
func (c *caller) attempt(ctx context.Context, m *method, target *url.URL, header http.Header, body []byte, contentType string, out proto.Message) (status int, failBody []byte, wait time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(ctx, m.call.GetMethod(), target.String(), rd)
	if err != nil {
		return 0, nil, 0, stop(callError(codes.InvalidArgument, c.client, ReasonBadRequest, transportText(err), nil))
	}
	r.Header = header.Clone()
	r.Header.Set("Accept", "application/json")
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	c.authorize(r)
	for _, h := range hooksFor(&before, m) {
		if err := h(ctx, r); err != nil {
			return 0, nil, 0, stop(callError(codes.Internal, c.client, "HOOK_REFUSED", "before-request hook: "+err.Error(), nil))
		}
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()
	for _, h := range hooksFor(&after, m) {
		if err := h(ctx, resp); err != nil {
			return 0, nil, 0, stop(callError(codes.Internal, c.client, "HOOK_REFUSED", "after-response hook: "+err.Error(), nil))
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return 0, nil, 0, err
	}
	if len(raw) > MaxResponseBytes {
		return 0, nil, 0, stop(callError(codes.Internal, c.client, ReasonTooLarge,
			"upstream body exceeds "+strconv.Itoa(MaxResponseBytes)+" bytes",
			map[string]string{"http_status": strconv.Itoa(resp.StatusCode)}))
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, raw, retryAfter(resp.Header.Get("Retry-After")), nil
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		span := trace.SpanFromContext(ctx)
		uo := wire.UnmarshalOptions{Report: func(i wire.Issue) {
			span.AddEvent("egress.decode_issue", trace.WithAttributes(
				attribute.String("path", i.Path), attribute.String("issue", i.Message)))
		}}
		if err := uo.Unmarshal(raw, out); err != nil {
			return 0, nil, 0, stop(callError(codes.Internal, c.client, ReasonUndecodable,
				"upstream success body does not decode: "+err.Error(),
				map[string]string{"http_status": strconv.Itoa(resp.StatusCode), "body": truncate(raw, maxErrorBody)}))
		}
	}
	return resp.StatusCode, nil, 0, nil
}

// transportText is a transport error without the request URL. net/http
// wraps it in a *url.Error whose text is the whole URL, query included —
// and an api_key credential travels in the query. The operation and the
// cause are what a reader needs; the host is on the span already.
func transportText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		inner := ue.Err
		var nested *url.Error
		for errors.As(inner, &nested) {
			inner = nested.Err
		}
		return ue.Op + ": " + inner.Error()
	}
	return err.Error()
}

// target is the request URL: the DSN's base, the path template filled in,
// the query.
func (c *caller) target(call *w17pb.EgressCall, req *wire.Request) (*url.URL, error) {
	path, err := fillPath(call.GetPath(), req.Path)
	if err != nil {
		return nil, err
	}
	u := *c.dsn.Base
	u.Path = c.dsn.Base.Path + path
	u.RawPath = c.dsn.Base.EscapedPath() + escapedPath(call.GetPath(), req.Path)
	q := req.Query
	if c.dsn.Auth == AuthAPIKey && c.dsn.KeyInQuery {
		q = cloneValues(q)
		q.Set(c.dsn.KeyName, c.dsn.Key)
	}
	u.RawQuery = q.Encode()
	return &u, nil
}

// fillPath substitutes every {name} of the template. A parameter the
// template names but the request leaves empty would address another
// resource (`/orders/` lists them), so it is refused.
func fillPath(tmpl string, params map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			b.WriteString(tmpl)
			return b.String(), nil
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			return "", errors.New("path template " + strconv.Quote(tmpl) + " has an unclosed {")
		}
		name := tmpl[i+1 : i+j]
		v, ok := params[name]
		if !ok || v == "" {
			return "", errors.New("path parameter " + strconv.Quote(name) + " is empty")
		}
		// A dot segment is resolved away by servers and proxies
		// (`/orders/..` is `/`): another resource, like an empty one.
		if v == "." || v == ".." {
			return "", errors.New("path parameter " + strconv.Quote(name) + " is a dot segment")
		}
		b.WriteString(tmpl[:i])
		b.WriteString(v)
		tmpl = tmpl[i+j+1:]
	}
}

// escapedPath is fillPath with each value escaped as one path segment, so a
// value holding `/` or `?` cannot reach another path.
func escapedPath(tmpl string, params map[string]string) string {
	esc := make(map[string]string, len(params))
	for k, v := range params {
		esc[k] = url.PathEscape(v)
	}
	s, _ := fillPath(tmpl, esc)
	return s
}

func encodeBody(call *w17pb.EgressCall, req *wire.Request) ([]byte, string, error) {
	if !req.HasBody() {
		return nil, "", nil
	}
	if call.GetRequestEncoding() == w17pb.EgressEncoding_EGRESS_ENCODING_FORM {
		vals, err := req.Form()
		if err != nil {
			return nil, "", err
		}
		return []byte(vals.Encode()), "application/x-www-form-urlencoded", nil
	}
	b, err := req.JSON()
	return b, "application/json", err
}

func (c *caller) authorize(r *http.Request) {
	switch c.dsn.Auth {
	case AuthBearer:
		r.Header.Set("Authorization", "Bearer "+c.dsn.Token)
	case AuthBasic:
		r.SetBasicAuth(c.dsn.User, c.dsn.Password)
	case AuthAPIKey:
		if !c.dsn.KeyInQuery {
			r.Header.Set(c.dsn.KeyName, c.dsn.Key)
		}
	}
}

// idempotent reports the methods a retry cannot repeat the effect of.
func idempotent(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

func retryStatus(s int) bool {
	return s == http.StatusTooManyRequests || s == http.StatusBadGateway ||
		s == http.StatusServiceUnavailable || s == http.StatusGatewayTimeout
}

// backoff is the wait after the attempt-th failure: 200 ms doubling, capped.
func backoff(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt && d < backoffCap; i++ {
		d *= 2
	}
	return min(d, backoffCap)
}

// retryAfter reads a Retry-After header: seconds or an HTTP date.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v)+1)
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
