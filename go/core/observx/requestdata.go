package observx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/lib/redact"
)

// Request data: what of a request an error report and a trace carry.
//
// An entry point stores a POINTER to the decoded request with [WithRequest] —
// nothing is encoded on the request path. Only when [ReportError] runs is the
// request written, by its redaction plan (sdk/go/lib/redact: no `sensitive`
// field, no unknown field, cut to [requestDataCeiling]), and attached:
//
//   - Sentry: a `request` context (method, type, the fields as JSON, what was
//     cut) and the tags `method`, `org_id`, `user_id`;
//   - OTel: an `observx.request` event on the failing span — never on a span
//     that did not fail;
//   - the stderr fallback line, when Sentry is off.
//
// W17_OBSERVX_REQUEST_DATA chooses how far the data travels:
//
//	all     (default) errors carry it, and so does every request's record in
//	        the request stream's telemetry sink, sampled by
//	        W17_OBSERVX_REQUEST_DATA_SAMPLE (0–1, default 1) — see [SampleRequestData]
//	errors  only errors carry it
//	off     nothing carries it; [WithRequest] stores nothing
//
// Spans are recorded at their own sampling either way; only the data is
// sampled, and an error carries it whenever the mode is not off.

// RequestDataMode is the value of W17_OBSERVX_REQUEST_DATA.
type RequestDataMode int

const (
	RequestDataAll RequestDataMode = iota
	RequestDataErrors
	RequestDataOff
)

func (m RequestDataMode) String() string {
	switch m {
	case RequestDataErrors:
		return "errors"
	case RequestDataOff:
		return "off"
	}
	return "all"
}

const (
	EnvRequestData       = "W17_OBSERVX_REQUEST_DATA"
	EnvRequestDataSample = "W17_OBSERVX_REQUEST_DATA_SAMPLE"
)

// requestDataCeiling bounds the encoded request an error carries: far under
// Sentry's event limit and an OTel attribute's practical size.
const requestDataCeiling = 16 << 10

var (
	requestMode   = RequestDataAll
	requestSample = 1.0
)

// readRequestDataEnv parses the two knobs; the caller holds mu.
func readRequestDataEnv(lookup func(string) string) (RequestDataMode, float64, error) {
	mode := RequestDataAll
	switch v := strings.ToLower(strings.TrimSpace(lookup(EnvRequestData))); v {
	case "", "all":
	case "errors":
		mode = RequestDataErrors
	case "off":
		mode = RequestDataOff
	default:
		return 0, 0, fmt.Errorf("observx: %s=%q: want all, errors or off", EnvRequestData, v)
	}
	sample := 1.0
	if v := strings.TrimSpace(lookup(EnvRequestDataSample)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 || f != f {
			return 0, 0, fmt.Errorf("observx: %s=%q: want a number from 0 to 1", EnvRequestDataSample, v)
		}
		sample = f
	}
	return mode, sample, nil
}

// RequestData reports the configured mode.
func RequestData() RequestDataMode {
	mu.Lock()
	defer mu.Unlock()
	return requestMode
}

// SampleRequestData decides, for one request, whether its data goes to the
// telemetry sink of the request stream: true in mode all with the sample's
// probability, false otherwise. Errors do not ask it.
func SampleRequestData() bool {
	mu.Lock()
	mode, sample := requestMode, requestSample
	mu.Unlock()
	if mode != RequestDataAll || sample <= 0 {
		return false
	}
	return sample >= 1 || rand.Float64() < sample //nolint:gosec // G404: a telemetry sample, nothing to guess
}

// requestLogger is the request telemetry sink's provider: the one MustInit
// installed, or one a binary brought ([UseRequestLogger]); nil = no sink.
var requestLogger otellog.LoggerProvider

// UseRequestLogger makes p the request event stream's telemetry sink — for a
// binary that runs its own OTel logs pipeline (MustInit installs one itself
// when it exports to OTel). nil removes it.
func UseRequestLogger(p otellog.LoggerProvider) {
	mu.Lock()
	defer mu.Unlock()
	requestLogger = p
}

// RequestLogger is the logger the request event stream's telemetry sink
// emits through; a no-op logger without a sink.
func RequestLogger() otellog.Logger {
	mu.Lock()
	p := requestLogger
	mu.Unlock()
	if p == nil {
		return lognoop.NewLoggerProvider().Logger("w17.requests")
	}
	return p.Logger("w17.requests")
}

// SampleRequestTelemetry decides, for one request, whether its record also
// goes to the telemetry backend: a sink is installed and SampleRequestData
// says yes. Without a sink it is false, so nothing is rendered for nobody.
func SampleRequestTelemetry() bool {
	mu.Lock()
	on := requestLogger != nil
	mu.Unlock()
	return on && SampleRequestData()
}

type requestKey struct{}

type requestInfo struct {
	method string
	req    proto.Message
}

// WithRequest records the request being served, for [ReportError] to attach
// if it is called for it. Only the pointer is stored; the caller must not
// change req afterwards. A nested entry point (an in-process call) replaces
// it with its own request for its own context. With the mode off it returns
// ctx unchanged.
func WithRequest(ctx context.Context, method string, req proto.Message) context.Context {
	if req == nil {
		return ctx
	}
	mu.Lock()
	off := requestMode == RequestDataOff
	mu.Unlock()
	if off {
		return ctx
	}
	return context.WithValue(ctx, requestKey{}, &requestInfo{method: method, req: req})
}

// requestReport is the request's data as an error carries it.
type requestReport struct {
	method, typ string
	data        json.RawMessage // nil when omitted
	cut         []string
	omitted     string // why there is no data
}

func requestReportOf(ctx context.Context) *requestReport {
	ri, _ := ctx.Value(requestKey{}).(*requestInfo)
	if ri == nil {
		return nil
	}
	mu.Lock()
	off := requestMode == RequestDataOff
	mu.Unlock()
	if off {
		return nil
	}
	r := &requestReport{method: ri.method, typ: string(ri.req.ProtoReflect().Descriptor().FullName())}
	wire, cut, err := redact.Default.Append(nil, ri.req, requestDataCeiling)
	switch {
	case errors.Is(err, redact.ErrNoPlan):
		r.omitted = "no redaction plan for the type"
		return r
	case err != nil:
		r.omitted = err.Error()
		return r
	}
	view := ri.req.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(wire, view); err != nil {
		r.omitted = "redacted record did not decode: " + err.Error()
		return r
	}
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(view)
	if err != nil {
		r.omitted = "redacted record did not render: " + err.Error()
		return r
	}
	r.data, r.cut = data, cut
	return r
}

func (r *requestReport) sentryContext() sentry.Context {
	c := sentry.Context{"method": r.method, "type": r.typ}
	if r.omitted != "" {
		c["omitted"] = r.omitted
		return c
	}
	var fields any
	if json.Unmarshal(r.data, &fields) == nil {
		c["data"] = fields
	}
	if len(r.cut) > 0 {
		c["truncated"] = r.cut
	}
	return c
}

func (r *requestReport) spanEvent(span trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.String("request.method", r.method),
		attribute.String("request.type", r.typ),
	}
	if r.omitted != "" {
		attrs = append(attrs, attribute.String("request.omitted", r.omitted))
	} else {
		attrs = append(attrs, attribute.String("request.data", string(r.data)))
		if len(r.cut) > 0 {
			attrs = append(attrs, attribute.StringSlice("request.truncated", r.cut))
		}
	}
	span.AddEvent("observx.request", trace.WithAttributes(attrs...))
}

func (r *requestReport) logSuffix() string {
	if r.omitted != "" {
		return fmt.Sprintf(" method=%s request=(omitted: %s)", r.method, r.omitted)
	}
	s := fmt.Sprintf(" method=%s request=%s", r.method, r.data)
	if len(r.cut) > 0 {
		s += " truncated=" + strings.Join(r.cut, ",")
	}
	return s
}

// callerTags are the request's caller, from the scopes the gateway threads
// to every tier (absent on an unauthenticated request or a project that does
// not declare the scope).
func callerTags(ctx context.Context, scope *sentry.Scope) {
	for _, name := range []string{"org_id", "user_id"} {
		if v, ok := principal.Scope(ctx, name); ok {
			scope.SetTag(name, v)
		}
	}
}
