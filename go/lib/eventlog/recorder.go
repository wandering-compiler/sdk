package eventlog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/redact"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The request event stream on the event bus: subject
// `w17.requests.<domain>.<api>`, stored in the W17_REQUESTS stream.
const (
	SubjectPrefix = "w17.requests"
	StreamName    = "W17_REQUESTS"
)

// Subject is a surface's subject.
func Subject(domain string, api w17pb.RequestApi) string {
	return SubjectPrefix + "." + domain + "." + apiToken(api)
}

func apiToken(api w17pb.RequestApi) string {
	return strings.ToLower(strings.TrimPrefix(api.String(), "REQUEST_API_"))
}

// EnvelopeMetaField is the envelope's RequestEventMeta field
// (w17.lock.requests.proto).
const EnvelopeMetaField protowire.Number = 50001

// DefaultRecordCeiling bounds one record's encoding.
const DefaultRecordCeiling = 64 << 10

// AuthFields are the numbers of the project's auth response fields the
// record reads the caller from: user_id is 2 by contract; scopes (a
// map<string,string>) and marketing_consent (a bool) are the project's, 0
// when it does not declare them.
type AuthFields struct {
	Scopes, MarketingConsent protowire.Number
}

// Purposes are what a surface's records serve.
type Purposes struct{ Marketing, Audit bool }

// Method is one recorded method's static settings (generated per surface).
type Method struct {
	// FullName is the backend method, `/<package>.<Service>/<Method>`.
	FullName string
	// Route is the route template on HTTP surfaces.
	Route string
	// RequestTag / ResponseTag are the envelope's oneof numbers;
	// ResponseTag 0 = the response is not recorded.
	RequestTag, ResponseTag protowire.Number
	// Drop: a marketing record does not wait for room.
	Drop bool
}

// RecorderConfig configures a [Recorder].
type RecorderConfig struct {
	Domain string
	// JS publishes the batches; NC gives the server's payload limit.
	JS jetstream.JetStream
	NC *nats.Conn
	// Purposes per surface with the stream on.
	Surfaces map[w17pb.RequestApi]Purposes
	Auth     AuthFields
	// RecordCeiling bounds one record; default DefaultRecordCeiling.
	RecordCeiling int
	// Stream bounds the W17_REQUESTS stream the recorder ensures.
	Stream StreamLimits
	// Fail overrides the failure path (tests); default: a report and a
	// structured log line per record, see [Recorder.fail].
	Fail func(Failure)
	// Pipeline settings, defaults as in [Config].
	BlockTimeout time.Duration
	QueueBytes   int64
	// Sink overrides the NATS sink (tests).
	Sink func(api w17pb.RequestApi) func(context.Context, *Batch) error
}

// Recorder records the requests of one gateway's surfaces.
type Recorder struct {
	domain   string
	ceiling  int
	auth     AuthFields
	surfaces map[w17pb.RequestApi]*Surface
	fail     func(Failure)

	conn *nats.Conn // owned when [Open] dialed it

	reported atomic.Int64 // last failure report, unix nanos
	failures atomic.Uint64
	consent  atomic.Int64 // last missing-consent report
}

// Surface records one surface's requests. A nil Surface records nothing,
// so a hook on a surface with the stream off costs a nil check.
type Surface struct {
	r        *Recorder
	api      w17pb.RequestApi
	purposes Purposes
	pipe     *Pipeline[*Record]
}

// NewRecorder ensures the stream and starts one pipeline per surface.
func NewRecorder(ctx context.Context, cfg RecorderConfig) (*Recorder, error) {
	if cfg.Domain == "" {
		return nil, errors.New("eventlog: Recorder needs the domain")
	}
	r := &Recorder{domain: cfg.Domain, ceiling: cfg.RecordCeiling, auth: cfg.Auth, surfaces: map[w17pb.RequestApi]*Surface{}}
	if r.ceiling <= 0 {
		r.ceiling = DefaultRecordCeiling
	}
	r.fail = cfg.Fail
	if r.fail == nil {
		r.fail = r.reportFailure
	}
	batchBytes := 1<<20 - natsHeaderRoom // NATS's default max_payload until the server says
	var ensure func(context.Context) error
	if cfg.Sink == nil {
		if cfg.JS == nil || cfg.NC == nil {
			return nil, errors.New("eventlog: Recorder needs a JetStream connection")
		}
		ensure = func(ctx context.Context) error {
			return EnsureStream(ctx, cfg.JS, StreamName, []string{SubjectPrefix + ".>"}, cfg.Stream)
		}
		// Not fatal: NATS may not be up yet. The sink creates the stream on
		// the first publish that finds none; until then batches take the
		// failure path, reported.
		ectx, cancel := context.WithTimeout(ctx, 2*time.Second) // a gateway's boot does not wait on NATS
		err := ensure(ectx)
		cancel()
		if err != nil {
			observx.ReportError(ctx, fmt.Errorf("eventlog: domain %s: the request stream is not ready yet, creating it on first publish: %w", cfg.Domain, err))
		}
		if mp := NATSBatchBytes(cfg.NC); mp > 64 {
			batchBytes = mp
		}
	}
	for api, purposes := range cfg.Surfaces {
		if !purposes.Marketing && !purposes.Audit {
			continue
		}
		sink := cfg.Sink
		var sf func(context.Context, *Batch) error
		if sink != nil {
			sf = sink(api)
		} else {
			sf = (&NATSSink{JS: cfg.JS, Subject: Subject(cfg.Domain, api), Ensure: ensure}).Sink
		}
		s := &Surface{r: r, api: api, purposes: purposes}
		p, err := New(Config[*Record]{
			Size:         recordSize,
			Encode:       r.encode,
			Sink:         sf,
			Fail:         r.fail,
			Tag:          func(rec *Record) string { return rec.orgID },
			BatchBytes:   batchBytes,
			BlockTimeout: cfg.BlockTimeout,
			QueueBytes:   cfg.QueueBytes,
		})
		if err != nil {
			return nil, err
		}
		s.pipe = p
		r.surfaces[api] = s
	}
	return r, nil
}

// Surface returns the recorder of one surface, nil when its stream is off
// (or r is nil).
func (r *Recorder) Surface(api w17pb.RequestApi) *Surface {
	if r == nil {
		return nil
	}
	return r.surfaces[api]
}

// Close drains every surface's pipeline, bounded by ctx, then closes the
// connection when the recorder owns it.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, s := range r.surfaces {
		if err := s.pipe.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if r.conn != nil {
		if err := r.conn.Drain(); err != nil {
			r.conn.Close()
		}
	}
	return errors.Join(errs...)
}

// Open dials NATS at dsn and starts a recorder over it, owning the
// connection. NATS not up yet is not an error: the connection keeps trying
// (and the stream is created on the first publish), and every drop and
// return is logged — a stream that stops must be visible in the log.
func Open(ctx context.Context, dsn string, cfg RecorderConfig) (*Recorder, error) {
	nc, err := nats.Connect(dsn,
		nats.Name("w17-requests-"+cfg.Domain),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.NoCallbacksAfterClientClose(),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("eventlog nats: connection LOST (%v) — request records wait, then take the failure path", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Printf("eventlog nats: reconnected to %s", c.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("eventlog: nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventlog: jetstream: %w", err)
	}
	cfg.NC, cfg.JS = nc, js
	r, err := NewRecorder(ctx, cfg)
	if err != nil {
		nc.Close()
		return nil, err
	}
	r.conn = nc
	return r, nil
}

// Record is one request on its way to the stream. The surface hook fills
// it between [Surface.Start] and [Record.Done]; the batcher builds the
// envelope from it, so the request path allocates nothing but the record.
type Record struct {
	s      *Surface
	method *Method
	start  time.Time
	dur    time.Duration

	req, resp  proto.Message
	httpStatus int32
	grpcCode   int32

	userID, orgID, traceID, requestID string
	span                              trace.SpanContext
	otel                              bool // sampled for the telemetry sink
	consent, consentKnown             bool
	purposes                          uint8 // bit 0 audit, bit 1 marketing

	ip, userAgent, referrer string
	utm                     map[string]string
}

const (
	purposeAudit uint8 = 1 << iota
	purposeMarketing
)

// Start begins a record of one request of m; nil when the surface records
// nothing (a nil Record's methods are no-ops).
func (s *Surface) Start(m *Method) *Record {
	if s == nil || m == nil {
		return nil
	}
	return &Record{s: s, method: m, start: time.Now()}
}

// SetClient records what the request says about the client.
func (rec *Record) SetClient(ip, userAgent, referrer string, utm map[string]string) {
	if rec == nil {
		return
	}
	rec.ip, rec.userAgent, rec.referrer, rec.utm = ip, userAgent, referrer, utm
}

// SetRequestID records the request id the surface assigned.
func (rec *Record) SetRequestID(id string) {
	if rec != nil {
		rec.requestID = id
	}
}

// SetRequest records the decoded request — as soon as it is decoded, so a
// request refused after decoding still carries it.
func (rec *Record) SetRequest(req proto.Message) {
	if rec != nil {
		rec.req = req
	}
}

// SetRequestAny is SetRequest for a gRPC interceptor's untyped request.
func (rec *Record) SetRequestAny(req any) {
	if m, ok := req.(proto.Message); ok {
		rec.SetRequest(m)
	}
}

func (rec *Record) setResultAny(resp any, err error) {
	m, _ := resp.(proto.Message)
	rec.SetResult(m, err)
}

// FinishUnary is SetResult and Finish for a gRPC interceptor: its untyped
// response and error, no HTTP status.
func (rec *Record) FinishUnary(ctx context.Context, resp any, err error) {
	if rec == nil {
		return
	}
	rec.setResultAny(resp, err)
	rec.Finish(ctx, 0)
}

// SetResult records the call's outcome: its error (the gRPC code) and,
// when the method opts in and the call succeeded, its response.
func (rec *Record) SetResult(resp proto.Message, err error) {
	if rec == nil {
		return
	}
	rec.grpcCode = int32(status.Code(err))
	if rec.method.ResponseTag != 0 && err == nil {
		rec.resp = resp
	}
}

// SetCaller records the verified caller from the metadata the gateway
// stamped (`x-w17-user`, `x-w17-scope-org_id`): the outgoing metadata it
// sends the backend, or the incoming metadata on the rpc surface.
func (rec *Record) SetCaller(md metadata.MD) {
	if rec != nil {
		rec.readCaller(md)
	}
}

// SetCallerOutgoing records the caller from the metadata ctx carries out —
// what an HTTP gateway stamped for the backend call.
func (rec *Record) SetCallerOutgoing(ctx context.Context) {
	if rec == nil {
		return
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		rec.readCaller(md)
	}
}

// SetCallerIncoming records the caller from the metadata ctx carries in —
// the rpc gateway's, after it stamped the verified principal.
func (rec *Record) SetCallerIncoming(ctx context.Context) {
	if rec == nil {
		return
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		rec.readCaller(md)
	}
}

// Finish closes the record and hands it to the stream; httpStatus is 0 on a
// non-HTTP surface. Nothing may touch the request or response after it.
func (rec *Record) Finish(ctx context.Context, httpStatus int) {
	if rec == nil {
		return
	}
	s := rec.s
	rec.dur = time.Since(rec.start)
	rec.httpStatus = int32(httpStatus)
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		rec.traceID = sc.TraceID().String()
		rec.span = sc
	}
	// The telemetry sink: W17_OBSERVX_REQUEST_DATA=all, sampled.
	rec.otel = observx.SampleRequestTelemetry()
	if s.purposes.Audit {
		rec.purposes |= purposeAudit
	}
	// Marketing records signed-in requests only, and needs the consent the
	// principal carries: a project that turned marketing on without giving
	// its auth response the field is misconfigured.
	if s.purposes.Marketing && rec.userID != "" {
		rec.purposes |= purposeMarketing
		if !rec.consentKnown {
			s.r.reportMissingConsent()
		}
	}
	if rec.purposes == 0 {
		return
	}
	if rec.method.Drop && rec.purposes&purposeAudit == 0 {
		_ = s.pipe.TrySubmit(rec)
		return
	}
	_ = s.pipe.Submit(ctx, rec)
}

// Done is SetResult, SetCaller and Finish in one, for a surface that has
// all of them at one place (the rpc interceptor, an MCP tool call).
func (rec *Record) Done(ctx context.Context, resp proto.Message, httpStatus int, err error, md metadata.MD) {
	rec.SetResult(resp, err)
	rec.SetCaller(md)
	rec.Finish(ctx, httpStatus)
}

// readCaller decodes the caller from the stamped metadata: the auth
// response's user_id (field 2) and marketing_consent, and the org_id scope.
func (rec *Record) readCaller(md metadata.MD) {
	if v := md.Get("x-w17-scope-org_id"); len(v) > 0 {
		rec.orgID = v[0]
	}
	v := md.Get("x-w17-user")
	if len(v) == 0 || v[0] == "" {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(v[0])
	if err != nil {
		return
	}
	auth := rec.s.r.auth
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return
		}
		raw = raw[n:]
		switch {
		case num == 2 && typ == protowire.BytesType:
			b, n := protowire.ConsumeBytes(raw)
			if n < 0 {
				return
			}
			rec.userID = string(b)
			raw = raw[n:]
			continue
		case auth.MarketingConsent != 0 && num == auth.MarketingConsent && typ == protowire.VarintType:
			x, n := protowire.ConsumeVarint(raw)
			if n < 0 {
				return
			}
			rec.consent, rec.consentKnown = x != 0, true
			raw = raw[n:]
			continue
		case auth.Scopes != 0 && num == auth.Scopes && typ == protowire.BytesType && rec.orgID == "":
			b, n := protowire.ConsumeBytes(raw)
			if n < 0 {
				return
			}
			if k, val := mapEntry(b); k == "org_id" {
				rec.orgID = val
			}
			raw = raw[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, raw)
		if n < 0 {
			return
		}
		raw = raw[n:]
	}
	if auth.MarketingConsent != 0 && !rec.consentKnown {
		rec.consentKnown = true // declared, unset: proto3's false
	}
}

func mapEntry(b []byte) (key, val string) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return
		}
		b = b[n:]
		if typ != protowire.BytesType {
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return
		}
		b = b[n:]
		switch num {
		case 1:
			key = string(v)
		case 2:
			val = string(v)
		}
	}
	return
}

func recordSize(rec *Record) int {
	n := 256 + len(rec.method.FullName) + len(rec.userAgent) + len(rec.referrer)
	if rec.req != nil {
		n += proto.Size(rec.req)
	}
	if rec.resp != nil {
		n += proto.Size(rec.resp)
	}
	return n
}

var metas = sync.Pool{New: func() any { return new(w17pb.RequestEventMeta) }}

// encode writes one envelope: the request and response by their
// redaction plans (cut to share the ceiling), then the meta.
func (r *Recorder) encode(dst []byte, rec *Record) ([]byte, error) {
	reqBudget := r.ceiling - 4<<10 // the meta's room
	if rec.resp != nil {
		reqBudget /= 2
	}
	var cut []string
	var err error
	var reqAt, respAt [2]int // the redacted messages' bytes in dst
	if rec.req != nil && rec.method.RequestTag != 0 {
		dst, cut, reqAt, err = appendRedacted(dst, rec.method.RequestTag, rec.req, reqBudget, "request.", cut)
		if err != nil {
			return dst, err
		}
	}
	if rec.resp != nil && rec.method.ResponseTag != 0 {
		dst, cut, respAt, err = appendRedacted(dst, rec.method.ResponseTag, rec.resp, reqBudget, "response.", cut)
		if err != nil {
			return dst, err
		}
	}
	m := metas.Get().(*w17pb.RequestEventMeta)
	defer func() {
		proto.Reset(m)
		metas.Put(m)
	}()
	id, idErr := uuid.NewV7()
	if idErr != nil {
		id = uuid.New()
	}
	m.Id = id[:]
	if rec.purposes&purposeAudit != 0 {
		m.Purposes = append(m.Purposes, w17pb.RequestPurpose_REQUEST_PURPOSE_AUDIT)
	}
	if rec.purposes&purposeMarketing != 0 {
		m.Purposes = append(m.Purposes, w17pb.RequestPurpose_REQUEST_PURPOSE_MARKETING)
	}
	m.Api = rec.s.api
	m.Domain = r.domain
	m.Method = rec.method.FullName
	m.Route = rec.method.Route
	m.HttpStatus = rec.httpStatus
	m.GrpcCode = rec.grpcCode
	m.Start = timestamppb.New(rec.start)
	m.DurationMicros = rec.dur.Microseconds()
	m.UserId = rec.userID
	m.OrgId = rec.orgID
	m.MarketingConsent = rec.consent
	if rec.ip != "" || rec.userAgent != "" || rec.referrer != "" || len(rec.utm) > 0 {
		m.Client = &w17pb.RequestClient{Ip: rec.ip, UserAgent: rec.userAgent, Referrer: rec.referrer, Utm: rec.utm}
	}
	m.Truncated = len(cut) > 0
	m.TruncatedPaths = cut
	m.TraceId = rec.traceID
	m.RequestId = rec.requestID
	if reqAt[1] > 0 {
		m.RequestField = int32(rec.method.RequestTag)
	}
	if respAt[1] > 0 {
		m.ResponseField = int32(rec.method.ResponseTag)
	}
	dst = protowire.AppendTag(dst, EnvelopeMetaField, protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(proto.Size(m)))
	dst, err = proto.MarshalOptions{}.MarshalAppend(dst, m)
	if err == nil && rec.otel {
		emitTelemetry(rec, m, dst[reqAt[0]:reqAt[1]], dst[respAt[0]:respAt[1]])
	}
	return dst, err
}

// appendRedacted writes msg as field num, redacted and cut to ceiling, and
// reports where the message's own bytes are in the result.
func appendRedacted(dst []byte, num protowire.Number, msg proto.Message, ceiling int, prefix string, cut []string) ([]byte, []string, [2]int, error) {
	dst = protowire.AppendTag(dst, num, protowire.BytesType)
	start := len(dst)
	dst = append(dst, 0, 0, 0) // room for a 3-byte length (up to 2 MiB)
	out, paths, err := redact.Default.Append(dst, msg, ceiling)
	if err != nil {
		return dst[:start-protowire.SizeTag(num)], cut, [2]int{}, fmt.Errorf("eventlog: %s%w", prefix, err)
	}
	n := len(out) - start - 3
	w := protowire.SizeVarint(uint64(n))
	if w > 3 {
		return dst[:start-protowire.SizeTag(num)], cut, [2]int{}, fmt.Errorf("eventlog: %s record over 2 MiB", prefix)
	}
	copy(out[start+w:], out[start+3:])
	out = out[:len(out)-(3-w)]
	protowire.AppendVarint(out[start:start], uint64(n))
	for _, p := range paths {
		cut = append(cut, prefix+p)
	}
	return out, cut, [2]int{start + w, start + w + n}, nil
}

// failureLineChunk keeps one log line under the json-file driver's 16 KiB.
const failureLineChunk = 12 << 10

// reportFailure is the default failure path: every record that was not
// delivered is written as structured log lines (its encoding, base64, in
// numbered parts — re-deliverable by hand, deduplicated by its id), and the
// failure is reported as an error at most once a second, with the count.
func (r *Recorder) reportFailure(f Failure) {
	total := r.failures.Add(uint64(f.Count))
	for _, rec := range f.Records {
		enc := base64.StdEncoding.EncodeToString(rec)
		parts := (len(enc) + failureLineChunk - 1) / failureLineChunk
		for i := 0; i < parts; i++ {
			end := min((i+1)*failureLineChunk, len(enc))
			log.Printf("eventlog: undelivered record domain=%s batch=%s reason=%q part=%d/%d data=%s",
				r.domain, f.BatchID, f.Err.Error(), i+1, parts, enc[i*failureLineChunk:end])
		}
	}
	if missing := f.Count - len(f.Records); missing > 0 {
		log.Printf("eventlog: %d undelivered record(s) without an encoding domain=%s reason=%q", missing, r.domain, f.Err.Error())
	}
	now := time.Now().UnixNano()
	if last := r.reported.Load(); now-last >= int64(time.Second) && r.reported.CompareAndSwap(last, now) {
		observx.ReportError(context.Background(), fmt.Errorf("eventlog: %d request record(s) not delivered (%d so far): %w", f.Count, total, f.Err))
	}
}

func (r *Recorder) reportMissingConsent() {
	now := time.Now().UnixNano()
	if last := r.consent.Load(); now-last >= int64(time.Minute) && r.consent.CompareAndSwap(last, now) {
		observx.ReportError(context.Background(), fmt.Errorf(
			"eventlog: domain %s records marketing, but the auth response carries no marketing_consent — every marketing record says no consent", r.domain))
	}
}
