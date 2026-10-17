package eventlog

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/lib/redact"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The request of these tests: a generated message whose field 7
// (default_value) its plan leaves out, holding the decoy.
func init() {
	redact.Register(map[string]redact.Plan{
		"google.protobuf.FieldDescriptorProto":     {Skip: []protowire.Number{7}},
		"google.protobuf.EnumValueDescriptorProto": {},
	})
}

const (
	reqTag       protowire.Number = 3
	respTag      protowire.Number = 4
	consentField protowire.Number = 5
	scopesField  protowire.Number = 7
)

var testMethod = &Method{FullName: "/acme.tasks.TaskService/Create", Route: "/tasks", RequestTag: reqTag, ResponseTag: respTag}

func sampleReq() *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{Name: proto.String("Write the report"), DefaultValue: proto.String("DECOY-secret")}
}

func sampleResp() *descriptorpb.EnumValueDescriptorProto {
	return &descriptorpb.EnumValueDescriptorProto{Name: proto.String("created"), Number: proto.Int32(42)}
}

// authMD is the metadata the gateway stamps for a verified caller.
func authMD(user string, consent bool, org string) metadata.MD {
	var raw []byte
	raw = protowire.AppendTag(raw, 1, protowire.VarintType) // an unrelated field
	raw = protowire.AppendVarint(raw, 9)
	raw = protowire.AppendTag(raw, 2, protowire.BytesType)
	raw = protowire.AppendString(raw, user)
	if consent {
		raw = protowire.AppendTag(raw, consentField, protowire.VarintType)
		raw = protowire.AppendVarint(raw, 1)
	}
	var entry []byte
	entry = protowire.AppendTag(entry, 1, protowire.BytesType)
	entry = protowire.AppendString(entry, "org_id")
	entry = protowire.AppendTag(entry, 2, protowire.BytesType)
	entry = protowire.AppendString(entry, org)
	raw = protowire.AppendTag(raw, scopesField, protowire.BytesType)
	raw = protowire.AppendBytes(raw, entry)
	return metadata.Pairs("x-w17-user", base64.StdEncoding.EncodeToString(raw))
}

type captured struct {
	mu      sync.Mutex
	batches map[w17pb.RequestApi][][]byte
	fails   []Failure
}

func (c *captured) sink(api w17pb.RequestApi) func(context.Context, *Batch) error {
	return func(_ context.Context, b *Batch) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := 0; i < b.Len(); i++ {
			c.batches[api] = append(c.batches[api], append([]byte(nil), b.Record(i)...))
		}
		return nil
	}
}

func (c *captured) records(api w17pb.RequestApi) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batches[api]
}

func newTestRecorder(t *testing.T, surfaces map[w17pb.RequestApi]Purposes, opts ...func(*RecorderConfig)) (*Recorder, *captured) {
	t.Helper()
	c := &captured{batches: map[w17pb.RequestApi][][]byte{}}
	cfg := RecorderConfig{
		Domain:   "tasks",
		Surfaces: surfaces,
		Auth:     AuthFields{Scopes: scopesField, MarketingConsent: consentField},
		Sink:     c.sink,
		Fail: func(f Failure) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.fails = append(c.fails, f)
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	r, err := NewRecorder(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func closeRecorder(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// envelope is a decoded record.
type envelope struct {
	meta      *w17pb.RequestEventMeta
	req, resp []byte
}

func decodeEnvelope(t *testing.T, b []byte) envelope {
	t.Helper()
	var e envelope
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("bad envelope")
		}
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			t.Fatalf("bad envelope field")
		}
		b = b[n:]
		switch num {
		case EnvelopeMetaField:
			e.meta = &w17pb.RequestEventMeta{}
			if err := proto.Unmarshal(v, e.meta); err != nil {
				t.Fatal(err)
			}
		case reqTag:
			e.req = v
		case respTag:
			e.resp = v
		default:
			t.Fatalf("unexpected envelope field %d", num)
		}
	}
	if e.meta == nil {
		t.Fatal("envelope without meta")
	}
	return e
}

func TestRecorder_WritesTheEnvelope(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true, Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	rec := s.Start(testMethod)
	rec.SetRequest(sampleReq())
	rec.SetClient("203.0.113.9", "curl/8", "https://example.com/", map[string]string{"source": "news"})
	rec.SetRequestID("req-1")
	rec.Done(context.Background(), sampleResp(), 200, nil, authMD("u-1", true, "org-9"))
	closeRecorder(t, r)

	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	if bytes.Contains(recs[0], []byte("DECOY")) {
		t.Fatal("a sensitive value reached the record")
	}
	e := decodeEnvelope(t, recs[0])
	m := e.meta
	if len(m.GetId()) != 16 || m.GetMethod() != testMethod.FullName || m.GetRoute() != "/tasks" ||
		m.GetDomain() != "tasks" || m.GetApi() != w17pb.RequestApi_REQUEST_API_REST ||
		m.GetHttpStatus() != 200 || m.GetGrpcCode() != 0 || m.GetStart() == nil ||
		m.GetUserId() != "u-1" || m.GetOrgId() != "org-9" || !m.GetMarketingConsent() ||
		m.GetRequestId() != "req-1" || m.GetClient().GetIp() != "203.0.113.9" ||
		m.GetClient().GetUtm()["source"] != "news" || m.GetTruncated() {
		t.Fatalf("meta %v", m)
	}
	if len(m.GetPurposes()) != 2 {
		t.Fatalf("purposes %v", m.GetPurposes())
	}
	if m.GetRequestField() != int32(reqTag) || m.GetResponseField() != int32(respTag) {
		t.Fatalf("fields %d/%d", m.GetRequestField(), m.GetResponseField())
	}
	var req descriptorpb.FieldDescriptorProto
	if err := proto.Unmarshal(e.req, &req); err != nil || req.GetName() != "Write the report" || req.DefaultValue != nil {
		t.Fatalf("request %v %v", &req, err)
	}
	var resp descriptorpb.EnumValueDescriptorProto
	if err := proto.Unmarshal(e.resp, &resp); err != nil || resp.GetNumber() != 42 {
		t.Fatalf("response %v %v", &resp, err)
	}
}

// Marketing records signed-in requests only; audit records every request.
func TestRecorder_PurposesPerRequest(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{
		w17pb.RequestApi_REQUEST_API_REST: {Marketing: true},
		w17pb.RequestApi_REQUEST_API_RPC:  {Audit: true, Marketing: true},
	})
	rest, rpc := r.Surface(w17pb.RequestApi_REQUEST_API_REST), r.Surface(w17pb.RequestApi_REQUEST_API_RPC)
	rest.Start(testMethod).Done(context.Background(), nil, 401, status.Error(codes.Unauthenticated, "no"), nil)
	rpc.Start(testMethod).Done(context.Background(), nil, 0, status.Error(codes.Unauthenticated, "no"), nil)
	rpc.Start(testMethod).Done(context.Background(), nil, 0, nil, authMD("u-2", false, "org-1"))
	if r.Surface(w17pb.RequestApi_REQUEST_API_MCP) != nil {
		t.Fatal("a surface with the stream off has a recorder")
	}
	r.Surface(w17pb.RequestApi_REQUEST_API_MCP).Start(testMethod).Done(context.Background(), nil, 0, nil, nil) // nil-safe
	closeRecorder(t, r)

	if n := len(c.records(w17pb.RequestApi_REQUEST_API_REST)); n != 0 {
		t.Fatalf("an anonymous request became a marketing record (%d)", n)
	}
	recs := c.records(w17pb.RequestApi_REQUEST_API_RPC)
	if len(recs) != 2 {
		t.Fatalf("%d rpc records", len(recs))
	}
	anon, signed := decodeEnvelope(t, recs[0]).meta, decodeEnvelope(t, recs[1]).meta
	if len(anon.GetPurposes()) != 1 || anon.GetPurposes()[0] != w17pb.RequestPurpose_REQUEST_PURPOSE_AUDIT ||
		anon.GetGrpcCode() != int32(codes.Unauthenticated) {
		t.Fatalf("anonymous audit record %v", anon)
	}
	if len(signed.GetPurposes()) != 2 || signed.GetMarketingConsent() {
		t.Fatalf("signed-in record %v", signed)
	}
}

func TestRecorder_ResponseOnlyWhenOptedInAndOK(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	noResp := *testMethod
	noResp.ResponseTag = 0
	s.Start(&noResp).Done(context.Background(), sampleResp(), 200, nil, nil)
	s.Start(testMethod).Done(context.Background(), sampleResp(), 500, status.Error(codes.Internal, "x"), nil)
	closeRecorder(t, r)
	for i, b := range c.records(w17pb.RequestApi_REQUEST_API_REST) {
		if decodeEnvelope(t, b).resp != nil {
			t.Fatalf("record %d carries a response", i)
		}
	}
}

func TestRecorder_CutPathsArePrefixed(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
		func(cfg *RecorderConfig) { cfg.RecordCeiling = 16 << 10 })
	req := sampleReq()
	req.Name = proto.String(strings.Repeat("n", 100_000))
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(testMethod)
	rec.SetRequest(req)
	rec.Done(context.Background(), nil, 200, nil, nil)
	closeRecorder(t, r)
	b := c.records(w17pb.RequestApi_REQUEST_API_REST)[0]
	if len(b) > 16<<10 {
		t.Fatalf("record %d bytes", len(b))
	}
	m := decodeEnvelope(t, b).meta
	if !m.GetTruncated() || len(m.GetTruncatedPaths()) != 1 || m.GetTruncatedPaths()[0] != "request.name" {
		t.Fatalf("meta %v", m)
	}
}

// A marketing record set to drop does not wait for room: with the sink
// stuck and the queue full it fails at once, the request going on.
func TestRecorder_DropDoesNotWait(t *testing.T) {
	gate := make(chan struct{})
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Marketing: true}},
		func(cfg *RecorderConfig) {
			cfg.QueueBytes, cfg.BlockTimeout = 1, time.Hour
			cfg.Sink = func(w17pb.RequestApi) func(context.Context, *Batch) error {
				return func(ctx context.Context, _ *Batch) error {
					select {
					case <-gate:
					case <-ctx.Done():
					}
					return nil
				}
			}
		})
	drop := *testMethod
	drop.Drop = true
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	s.Start(&drop).Done(context.Background(), nil, 200, nil, authMD("u", true, "o")) // takes the queue
	t0 := time.Now()
	s.Start(&drop).Done(context.Background(), nil, 200, nil, authMD("u", true, "o"))
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("a drop record waited %v", d)
	}
	c.mu.Lock()
	failed := len(c.fails)
	c.mu.Unlock()
	if failed != 1 {
		t.Fatalf("%d failures, want the second record failed", failed)
	}
	close(gate)
	closeRecorder(t, r)
}

// The default failure path writes each undelivered record as log lines
// under the log driver's line limit, numbered so it can be read back.
func TestRecorder_FailureLogLines(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	r := &Recorder{domain: "tasks"}
	record := bytes.Repeat([]byte{0xAB}, 20_000)
	r.reportFailure(Failure{Err: ErrQueueFull, Records: [][]byte{record}, Count: 2})
	var data strings.Builder
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	parts := 0
	for _, l := range lines {
		if len(l) > 16<<10 {
			t.Fatalf("a log line of %d bytes", len(l))
		}
		if i := strings.Index(l, " data="); i >= 0 {
			parts++
			data.WriteString(l[i+len(" data="):])
		}
	}
	if parts != 3 || !strings.Contains(buf.String(), "part=3/3") || !strings.Contains(buf.String(), "1 undelivered record(s) without an encoding") {
		t.Fatalf("lines:\n%s", buf.String())
	}
	back, err := base64.StdEncoding.DecodeString(data.String())
	if err != nil || !bytes.Equal(back, record) {
		t.Fatal("the record does not read back from its log lines")
	}
}

// End to end on JetStream: the recorder ensures the stream and publishes
// on w17.requests.<domain>.<api>, the batch's organizations on its header.
func TestRecorder_OverJetStream(t *testing.T) {
	nc, js := embeddedJetStream(t)
	r, err := NewRecorder(context.Background(), RecorderConfig{
		Domain: "tasks", JS: js, NC: nc,
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_RPC: {Audit: true}},
		Auth:     AuthFields{Scopes: scopesField},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Surface(w17pb.RequestApi_REQUEST_API_RPC).Start(testMethod).Done(context.Background(), nil, 0, nil, authMD("u-1", false, "org-7"))
	closeRecorder(t, r)
	stream, err := js.Stream(context.Background(), StreamName)
	if err != nil {
		t.Fatal(err)
	}
	m, err := stream.GetLastMsgForSubject(context.Background(), "w17.requests.tasks.rpc")
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get(HeaderOrgs) != "org-7" || m.Header.Get(jetstream.MsgIDHeader) == "" {
		t.Fatalf("headers %v", m.Header)
	}
	_, recs := decodeBatch(t, m.Data)
	if len(recs) != 1 || decodeEnvelope(t, recs[0]).meta.GetUserId() != "u-1" {
		t.Fatalf("records %d", len(recs))
	}
}

func TestSubject(t *testing.T) {
	if got := Subject("tasks", w17pb.RequestApi_REQUEST_API_ADMIN); got != "w17.requests.tasks.admin" {
		t.Fatal(got)
	}
}

// Open dials NATS itself; with the server not up yet it still starts, and
// records once the server is there.
func TestOpen_StartsBeforeNATSAndOwnsTheConnection(t *testing.T) {
	r, err := Open(context.Background(), "nats://127.0.0.1:1", RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
		Fail:     func(Failure) {},
	})
	if err != nil {
		t.Fatalf("Open with NATS down: %v", err)
	}
	if r.Surface(w17pb.RequestApi_REQUEST_API_REST) == nil {
		t.Fatal("no surface")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.Close(ctx)
	if !r.conn.IsClosed() {
		t.Fatal("Close left the owned connection open")
	}
}

func TestRecord_SetCallerFromContext(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	out := s.Start(testMethod)
	out.SetCallerOutgoing(metadata.NewOutgoingContext(context.Background(), authMD("u-out", false, "o")))
	out.Finish(context.Background(), 200)
	in := s.Start(testMethod)
	in.SetCallerIncoming(metadata.NewIncomingContext(context.Background(), authMD("u-in", false, "o")))
	in.Finish(context.Background(), 0)
	var nilRec *Record
	nilRec.SetCallerOutgoing(context.Background())
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 2 || decodeEnvelope(t, recs[0]).meta.GetUserId() != "u-out" || decodeEnvelope(t, recs[1]).meta.GetUserId() != "u-in" {
		t.Fatalf("%d records", len(recs))
	}
}

func TestRecord_InterceptorShape(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_RPC: {Audit: true}})
	rec := r.Surface(w17pb.RequestApi_REQUEST_API_RPC).Start(testMethod)
	rec.SetRequestAny(sampleReq())
	rec.SetRequestAny("not a message") // ignored, keeps the request
	rec.FinishUnary(context.Background(), sampleResp(), nil)
	var none *Record
	none.FinishUnary(context.Background(), nil, nil)
	closeRecorder(t, r)
	e := decodeEnvelope(t, c.records(w17pb.RequestApi_REQUEST_API_RPC)[0])
	if e.req == nil || e.resp == nil || e.meta.GetHttpStatus() != 0 {
		t.Fatalf("envelope %+v", e)
	}
}
