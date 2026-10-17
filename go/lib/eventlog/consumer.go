package eventlog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/wandering-compiler/sdk/go/core/observx"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The consumer contract: a project's marketing and audit integrations read
// the request event stream through these interfaces. A purpose is a METHOD,
// so the compiler tells them apart — a marketing consumer cannot be
// registered as an audit one by mistake; a type serving both implements
// both, deliberately. Each consumer receives only the records whose purposes
// include its own, from its own durable consumer, so one consumer failing
// never redelivers a batch to another.

// MarketingConsumer receives the records that serve marketing (signed-in
// requests, with the caller's consent).
type MarketingConsumer interface {
	ConsumeMarketing(ctx context.Context, b *ConsumedBatch) error
}

// AuditConsumer receives the records that serve audit (every request).
type AuditConsumer interface {
	ConsumeAudit(ctx context.Context, b *ConsumedBatch) error
}

// ConsumedBatch is one batch as a consumer reads it: the records of its
// purpose, in the order they were recorded. Delivery is at least once — a
// consumer that meets a record id twice skips it.
type ConsumedBatch struct {
	// ID is the batch id (a UUIDv7).
	ID uuid.UUID
	// Subject is where it was published: w17.requests.<domain>.<api>.
	Subject string
	Records []ConsumedRecord
}

// ConsumedRecord is one request's record.
type ConsumedRecord struct {
	Meta *w17pb.RequestEventMeta
	// Request and Response are decoded with the method's own types when this
	// binary links them (they register in the protobuf registry); nil when it
	// does not, or when the record carries none. RequestRaw / ResponseRaw are
	// their bytes either way.
	Request, Response       proto.Message
	RequestRaw, ResponseRaw []byte
}

// Consumers is the set of registered consumers. The generated event
// subscriber hands one to the project's registration function; a binary of
// the project's own builds one and calls [Consumers.Run].
type Consumers struct {
	mu   sync.Mutex
	list []registered
}

type registered struct {
	name     string
	purpose  w17pb.RequestPurpose
	consume  func(context.Context, *ConsumedBatch) error
	subjects []string
}

var consumerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Marketing registers c under name — the stable identity its delivery
// progress is kept under (renaming it starts over). subjects narrows what
// it reads (`w17.requests.<domain>.>`); none = every domain's.
func (cs *Consumers) Marketing(name string, c MarketingConsumer, subjects ...string) error {
	return cs.add(name, w17pb.RequestPurpose_REQUEST_PURPOSE_MARKETING, c.ConsumeMarketing, subjects)
}

// Audit registers c under name, as Marketing.
func (cs *Consumers) Audit(name string, c AuditConsumer, subjects ...string) error {
	return cs.add(name, w17pb.RequestPurpose_REQUEST_PURPOSE_AUDIT, c.ConsumeAudit, subjects)
}

func (cs *Consumers) add(name string, p w17pb.RequestPurpose, f func(context.Context, *ConsumedBatch) error, subjects []string) error {
	if !consumerName.MatchString(name) {
		return fmt.Errorf("eventlog: consumer name %q: want 1–64 of A–Z a–z 0–9 _ -", name)
	}
	for _, s := range subjects {
		if s != SubjectPrefix+".>" && !strings.HasPrefix(s, SubjectPrefix+".") {
			return fmt.Errorf("eventlog: consumer %s: subject %q is not under %s", name, s, SubjectPrefix)
		}
	}
	if len(subjects) == 0 {
		subjects = []string{SubjectPrefix + ".>"}
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, r := range cs.list {
		if r.name == name && r.purpose == p {
			return fmt.Errorf("eventlog: consumer %s registered twice for %s", name, p)
		}
	}
	cs.list = append(cs.list, registered{name: name, purpose: p, consume: f, subjects: subjects})
	return nil
}

// Len is the number of registered consumers.
func (cs *Consumers) Len() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.list)
}

// ConsumerConfig configures [Consumers.Run].
type ConsumerConfig struct {
	JS jetstream.JetStream
	// DurablePrefix prefixes each consumer's durable name
	// (`<prefix>-<purpose>-<name>`), so two deployments do not share
	// progress. Required.
	DurablePrefix string
	// MaxDeliver is how often a batch is offered before it goes to the dead
	// letter subject. Default 5.
	MaxDeliver int
	// AckWait bounds one Consume call. Default 30s.
	AckWait time.Duration
	// Backoff is the wait before a failed batch is offered again. Default 1s,
	// doubling to 1m.
	Backoff func(attempt int) time.Duration
	// Stream bounds the dead letter stream. Defaults as [StreamLimits], but
	// MaxBytes [DeadLetterMaxBytes]: it holds what consumers failed on, not
	// the traffic.
	Stream StreamLimits
}

// DeadLetterPrefix is where a batch goes after MaxDeliver failed offers:
// `_dlq.w17.requests.<purpose>.<name>`, outside the stream's own subjects so
// it is never consumed again by the same consumer.
const DeadLetterPrefix = "_dlq." + SubjectPrefix

// DeadLetterStream holds them.
const DeadLetterStream = StreamName + "_DLQ"

// DeadLetterMaxBytes is the dead letter stream's default cap.
const DeadLetterMaxBytes = 1 << 30

// Run consumes for every registered consumer until ctx ends. Each has its
// own durable consumer on the request stream: a batch is acknowledged once
// its Consume returned nil, offered again after a backoff when it returned
// an error, and moved to the dead letter subject (and reported) after
// MaxDeliver offers. A batch with no record of the consumer's purpose is
// acknowledged without calling it.
func (cs *Consumers) Run(ctx context.Context, cfg ConsumerConfig) error {
	if cfg.JS == nil || cfg.DurablePrefix == "" {
		return errors.New("eventlog: Run needs JetStream and a DurablePrefix")
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = 5
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = 30 * time.Second
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(attempt int) time.Duration {
			d := time.Second << max(attempt-1, 0)
			return min(d, time.Minute)
		}
	}
	cs.mu.Lock()
	list := append([]registered(nil), cs.list...)
	cs.mu.Unlock()
	if len(list) == 0 {
		<-ctx.Done()
		return nil
	}
	if err := EnsureStream(ctx, cfg.JS, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{}); err != nil {
		return err
	}
	dl := cfg.Stream
	if dl.MaxBytes <= 0 {
		dl.MaxBytes = DeadLetterMaxBytes
	}
	if err := EnsureStream(ctx, cfg.JS, DeadLetterStream, []string{DeadLetterPrefix + ".>"}, dl); err != nil {
		return err
	}
	stream, err := cfg.JS.Stream(ctx, StreamName)
	if err != nil {
		return fmt.Errorf("eventlog: %w", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(list))
	for _, r := range list {
		purpose := strings.ToLower(strings.TrimPrefix(r.purpose.String(), "REQUEST_PURPOSE_"))
		durable := cfg.DurablePrefix + "-" + purpose + "-" + r.name
		cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable:        durable,
			FilterSubjects: r.subjects,
			AckPolicy:      jetstream.AckExplicitPolicy,
			AckWait:        cfg.AckWait,
			MaxDeliver:     cfg.MaxDeliver,
			DeliverPolicy:  jetstream.DeliverAllPolicy,
		})
		if err != nil {
			return fmt.Errorf("eventlog: consumer %s: %w", durable, err)
		}
		dlq := DeadLetterPrefix + "." + purpose + "." + r.name
		wg.Add(1)
		go func(r registered) {
			defer wg.Done()
			errs <- consumeLoop(ctx, cons, r, cfg, dlq)
		}(r)
	}
	wg.Wait()
	close(errs)
	var all []error
	for e := range errs {
		if e != nil {
			all = append(all, e)
		}
	}
	return errors.Join(all...)
}

func consumeLoop(ctx context.Context, cons jetstream.Consumer, r registered, cfg ConsumerConfig, dlq string) error {
	for ctx.Err() == nil {
		batch, err := cons.Fetch(16, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("eventlog: consumer %s: fetch: %v", r.name, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		for msg := range batch.Messages() {
			handleMessage(ctx, msg, r, cfg, dlq)
		}
	}
	return nil
}

func handleMessage(ctx context.Context, msg jetstream.Msg, r registered, cfg ConsumerConfig, dlq string) {
	b, err := DecodeBatch(msg.Subject(), msg.Data(), r.purpose)
	if err != nil {
		// A batch that does not decode never will: dead-letter it now.
		deadLetter(ctx, msg, cfg, dlq, r.name, err)
		return
	}
	if len(b.Records) == 0 {
		_ = msg.Ack()
		return
	}
	cerr := callConsumer(ctx, r, b, cfg.AckWait)
	if cerr == nil {
		_ = msg.Ack()
		return
	}
	attempt := 1
	if md, err := msg.Metadata(); err == nil {
		attempt = int(md.NumDelivered)
	}
	if attempt >= cfg.MaxDeliver {
		deadLetter(ctx, msg, cfg, dlq, r.name, cerr)
		return
	}
	_ = msg.NakWithDelay(cfg.Backoff(attempt))
}

// callConsumer runs one Consume, bounded by the ack wait; a panic is an
// error like any other.
func callConsumer(ctx context.Context, r registered, b *ConsumedBatch, wait time.Duration) (err error) {
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w in consumer %s: %v", ErrPanic, r.name, p)
		}
	}()
	return r.consume(cctx, b)
}

func deadLetter(ctx context.Context, msg jetstream.Msg, cfg ConsumerConfig, dlq, name string, cause error) {
	out := &nats.Msg{Subject: dlq, Data: msg.Data(), Header: nats.Header{}}
	for k, v := range msg.Headers() {
		out.Header[k] = v
	}
	out.Header.Set("W17-Dead-Letter-Reason", cause.Error())
	out.Header.Set("W17-Dead-Letter-Subject", msg.Subject())
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cfg.JS.PublishMsg(pctx, out); err != nil {
		// Not stored: leave it to be offered again rather than lose it.
		_ = msg.Nak()
		observx.ReportError(ctx, fmt.Errorf("eventlog: consumer %s: dead-lettering a batch failed (%v) after: %w", name, err, cause))
		return
	}
	_ = msg.Term()
	observx.ReportError(ctx, fmt.Errorf("eventlog: consumer %s: a batch moved to %s: %w", name, dlq, cause))
}

// DecodeBatch reads a published batch, keeping the records that serve
// purpose (REQUEST_PURPOSE_UNSPECIFIED keeps all).
func DecodeBatch(subject string, wire []byte, purpose w17pb.RequestPurpose) (*ConsumedBatch, error) {
	b := &ConsumedBatch{Subject: subject}
	for len(wire) > 0 {
		num, typ, n := protowire.ConsumeTag(wire)
		if n < 0 {
			return nil, errors.New("eventlog: batch: bad tag")
		}
		wire = wire[n:]
		if typ != protowire.BytesType {
			n = protowire.ConsumeFieldValue(num, typ, wire)
			if n < 0 {
				return nil, errors.New("eventlog: batch: bad field")
			}
			wire = wire[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(wire)
		if n < 0 {
			return nil, errors.New("eventlog: batch: bad field")
		}
		wire = wire[n:]
		switch num {
		case BatchFieldID:
			if len(v) == 16 {
				copy(b.ID[:], v)
			}
		case BatchFieldRecords:
			rec, err := decodeRecord(v)
			if err != nil {
				return nil, err
			}
			if purpose == w17pb.RequestPurpose_REQUEST_PURPOSE_UNSPECIFIED || hasPurpose(rec.Meta, purpose) {
				b.Records = append(b.Records, rec)
			}
		}
	}
	return b, nil
}

func hasPurpose(m *w17pb.RequestEventMeta, p w17pb.RequestPurpose) bool {
	for _, x := range m.GetPurposes() {
		if x == p {
			return true
		}
	}
	return false
}

func decodeRecord(b []byte) (ConsumedRecord, error) {
	var rec ConsumedRecord
	fields := map[protowire.Number][]byte{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return rec, errors.New("eventlog: record: bad tag")
		}
		b = b[n:]
		if typ != protowire.BytesType {
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return rec, errors.New("eventlog: record: bad field")
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return rec, errors.New("eventlog: record: bad field")
		}
		b = b[n:]
		fields[num] = v
	}
	raw, ok := fields[EnvelopeMetaField]
	if !ok {
		return rec, errors.New("eventlog: record without its meta")
	}
	rec.Meta = &w17pb.RequestEventMeta{}
	if err := proto.Unmarshal(raw, rec.Meta); err != nil {
		return rec, fmt.Errorf("eventlog: record meta: %w", err)
	}
	if f := rec.Meta.GetRequestField(); f != 0 {
		rec.RequestRaw = fields[protowire.Number(f)]
	}
	if f := rec.Meta.GetResponseField(); f != 0 {
		rec.ResponseRaw = fields[protowire.Number(f)]
	}
	in, out := methodTypes(rec.Meta.GetMethod())
	rec.Request = decodeAs(in, rec.RequestRaw)
	rec.Response = decodeAs(out, rec.ResponseRaw)
	return rec, nil
}

// methodTypes finds a method's request and response types in the protobuf
// registry (`/<package>.<Service>/<Method>`); nil when not linked.
func methodTypes(method string) (in, out protoreflect.MessageType) {
	svc, name, ok := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	if !ok {
		return nil, nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil, nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil
	}
	md := sd.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return nil, nil
	}
	in, _ = protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
	out, _ = protoregistry.GlobalTypes.FindMessageByName(md.Output().FullName())
	return in, out
}

func decodeAs(t protoreflect.MessageType, raw []byte) proto.Message {
	if t == nil || raw == nil {
		return nil
	}
	m := t.New().Interface()
	if proto.Unmarshal(raw, m) != nil {
		return nil
	}
	return m
}

// Serve dials NATS at dsn and runs the consumers until ctx ends — what the
// generated subscriber calls. NATS not up yet is not an error: the
// connection keeps trying.
func (cs *Consumers) Serve(ctx context.Context, dsn string, cfg ConsumerConfig) error {
	if dsn == "" {
		return errors.New("eventlog: request consumers need a NATS DSN (the request stream lives in JetStream)")
	}
	nc, err := nats.Connect(dsn,
		nats.Name("w17-request-consumers"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.NoCallbacksAfterClientClose(),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Printf("eventlog consumers nats: connection LOST (%v) — consuming stops until it returns", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Printf("eventlog consumers nats: reconnected to %s", c.ConnectedUrl())
		}),
	)
	if err != nil {
		return fmt.Errorf("eventlog: nats connect: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("eventlog: jetstream: %w", err)
	}
	cfg.JS = js
	// The stream may not exist yet (no gateway has recorded): wait for it
	// rather than fail the subscriber's boot.
	for {
		err = cs.Run(ctx, cfg)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		log.Printf("eventlog: request consumers: %v — retrying in 5s", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
