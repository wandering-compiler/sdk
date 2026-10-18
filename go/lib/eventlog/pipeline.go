// Package eventlog carries the request event stream: every request a gateway
// serves, as a record, to the sinks that want it (the event bus for marketing
// and audit consumers, OTel for the telemetry backend) — without a goroutine
// per request and without ever failing or blocking a request for long.
//
// [Pipeline] is the two-stage pipeline the generated gateway layer feeds:
//
//	request ── Submit (a pointer, bounded wait) ──▶ queue
//	        ── batcher: encode each record, close a batch by count / period / size
//	        ── small queue of batches ──▶ sender: Sink(batch)
//
// Nothing is dropped while there is room: a full queue BLOCKS the request
// until there is, and the queue is bounded in BYTES (Config.Size, measured
// on the request path). The wait is bounded too (Config.BlockTimeout and the
// request's context): after it the record takes the failure path and the
// request goes on. Every record that is not delivered — the wait ran out, it
// did not encode, it does not fit a batch, the sink refused its batch, the
// pipeline was closed — reaches Config.Fail with the reason, so it is always
// reported and never silently lost. A panic in the encoder, the sink or Fail
// is recovered and counted; the stages keep running.
//
// [Pipeline.Close] stops accepting and drains both stages into the sink,
// bounded by its context; what is left when the context ends goes to Fail.
//
// # The batch on the wire
//
// A batch is the protobuf encoding of a message whose field 1 is the batch id
// (16 bytes, a UUIDv7 — a sink uses it to deduplicate a re-sent batch) and
// whose field 2 repeats the records, each the bytes Config.Encode wrote:
//
//	message Batch {
//	  bytes id = 1;
//	  repeated <Envelope> records = 2;
//	}
//
// so a consumer decodes it with its own Batch type, whatever the record type.
package eventlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// Field numbers of the batch message (see the package doc).
const (
	BatchFieldID      protowire.Number = 1
	BatchFieldRecords protowire.Number = 2
)

// Why a record took the failure path.
var (
	ErrQueueFull = errors.New("eventlog: no room in the queue within the wait")
	ErrClosed    = errors.New("eventlog: pipeline closed")
	ErrTooLarge  = errors.New("eventlog: record does not fit a batch")
	ErrPanic     = errors.New("eventlog: panic")
)

// Config configures a [Pipeline]. Size, Encode, Sink, Fail and BatchBytes
// are required.
type Config[T any] struct {
	// Size is what an item holds in memory while queued, in bytes
	// (proto.Size of its messages). It runs on the request path.
	Size func(T) int
	// Encode appends one record's wire form to dst (the redacted envelope).
	Encode func(dst []byte, item T) ([]byte, error)
	// Sink delivers a batch. The batch's buffer is reused once Sink returns:
	// a sink must not keep it.
	Sink func(ctx context.Context, b *Batch) error
	// Fail receives every record that was not delivered, with the reason.
	Fail func(Failure)
	// Tag, when set, names what a record belongs to (its organization); a
	// batch carries the distinct tags of its records ([Batch.Tags]), so a
	// sink can label it and a consumer skip it without decoding.
	Tag func(T) string

	// BatchBytes bounds a batch's encoded size (the sink's limit — NATS's
	// max_payload, less the room its headers take).
	BatchBytes int
	// BatchItems closes a batch at this many records. Default 1000.
	BatchItems int
	// BatchPeriod closes a batch this long after its first record.
	// Default 10ms.
	BatchPeriod time.Duration
	// QueueBytes bounds the memory held by queued and in-flight records,
	// by Size. Default 64 MiB.
	QueueBytes int64
	// QueueItems is the capacity of the request-side queue. Default 65536.
	QueueItems int
	// SendQueue is how many closed batches wait for the sender. Default 4.
	SendQueue int
	// BlockTimeout bounds how long Submit waits for room. Default 1s.
	BlockTimeout time.Duration
	// RetryFor bounds how long the sender retries a batch the sink refused
	// (with backoff; a sink deduplicating by batch id makes it safe) before
	// the batch takes the failure path. Default 5s; negative = no retry.
	RetryFor time.Duration
}

// Batch is one closed batch, as the sink receives it.
type Batch struct {
	ID uuid.UUID
	// Wire is the encoded batch message (see the package doc).
	Wire []byte

	// Tags are the distinct non-empty tags of its records (Config.Tag), at
	// most MaxBatchTags; TagsOverflow says there were more, so Tags is not
	// the whole set.
	Tags         []string
	TagsOverflow bool

	offsets []int        // each record's [start,end) in Wire, flattened
	dones   []chan error // SubmitWait callers waiting on this batch
	held    int64        // queue bytes its records hold
}

// MaxBatchTags bounds [Batch.Tags].
const MaxBatchTags = 32

func (b *Batch) addTag(tag string) {
	if tag == "" || b.TagsOverflow {
		return
	}
	for _, t := range b.Tags {
		if t == tag {
			return
		}
	}
	if len(b.Tags) == MaxBatchTags {
		b.TagsOverflow, b.Tags = true, nil
		return
	}
	b.Tags = append(b.Tags, tag)
}

// Len is the number of records in the batch.
func (b *Batch) Len() int { return len(b.offsets) / 2 }

// Record returns the i-th record's encoding (a sub-slice of Wire).
func (b *Batch) Record(i int) []byte { return b.Wire[b.offsets[2*i]:b.offsets[2*i+1]] }

// Failure is a set of records that will not be delivered.
type Failure struct {
	Err error
	// BatchID is the batch they were in; zero when they never reached one.
	BatchID uuid.UUID
	// Records are the records' encodings, when they were encoded; empty for
	// a record whose encoding itself failed. Valid only during the call.
	Records [][]byte
	// Count is how many records failed (also those without an encoding).
	Count int
}

// Stats are the pipeline's counters since it started.
type Stats struct {
	Submitted, Delivered, Failed, Batches, Waited, Panics uint64
}

type queued[T any] struct {
	item T
	size int64
	done chan error // SubmitWait's: told the batch's outcome; nil otherwise
}

// Pipeline is the two-stage pipeline (see the package doc).
type Pipeline[T any] struct {
	cfg  Config[T]
	sem  *semaphore.Weighted
	in   chan queued[T]
	out  chan *Batch
	bufs sync.Pool

	closeMu sync.RWMutex // Submit holds R while it may send on in
	closed  bool

	sinkCtx    context.Context
	cancelSink context.CancelFunc
	aborted    atomic.Bool
	done       chan struct{}

	submitted, delivered, failed, batches, waited, panics atomic.Uint64
	lastPanicReport                                       atomic.Int64
}

// recovered counts a recovered panic and reports it — at most once a second,
// so a panicking encoder does not become one report per record.
func (p *Pipeline[T]) recovered(where string, r any) error {
	p.panics.Add(1)
	err := fmt.Errorf("%w in %s: %v", ErrPanic, where, r)
	now := time.Now().UnixNano()
	if last := p.lastPanicReport.Load(); now-last >= int64(time.Second) && p.lastPanicReport.CompareAndSwap(last, now) {
		observx.ReportError(context.Background(), fmt.Errorf("eventlog: %w (%d so far)", err, p.panics.Load()))
	}
	return err
}

// New starts a pipeline.
func New[T any](cfg Config[T]) (*Pipeline[T], error) {
	switch {
	case cfg.Size == nil || cfg.Encode == nil || cfg.Sink == nil || cfg.Fail == nil:
		return nil, errors.New("eventlog: Size, Encode, Sink and Fail are required")
	case cfg.BatchBytes <= 64:
		return nil, fmt.Errorf("eventlog: BatchBytes %d is too small", cfg.BatchBytes)
	}
	if cfg.BatchItems <= 0 {
		cfg.BatchItems = 1000
	}
	if cfg.BatchPeriod <= 0 {
		cfg.BatchPeriod = 10 * time.Millisecond
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = 64 << 20
	}
	if cfg.QueueItems <= 0 {
		cfg.QueueItems = 65536
	}
	if cfg.SendQueue <= 0 {
		cfg.SendQueue = 4
	}
	if cfg.BlockTimeout <= 0 {
		cfg.BlockTimeout = time.Second
	}
	if cfg.RetryFor == 0 {
		cfg.RetryFor = 5 * time.Second
	}
	p := &Pipeline[T]{
		cfg:  cfg,
		sem:  semaphore.NewWeighted(cfg.QueueBytes),
		in:   make(chan queued[T], cfg.QueueItems),
		out:  make(chan *Batch, cfg.SendQueue),
		done: make(chan struct{}),
	}
	p.bufs.New = func() any { b := make([]byte, 0, cfg.BatchBytes); return &b }
	p.sinkCtx, p.cancelSink = context.WithCancel(context.Background()) //nolint:gosec // G118: cancelSink is stored and called by Close; not leaked.
	go p.batcher()
	go p.sender()
	return p, nil
}

// Submit queues item, waiting for room at most BlockTimeout or until ctx
// ends. A nil return means the item is queued; otherwise it has already gone
// to Fail and the error says why — the caller goes on either way. The item
// must not change after Submit.
func (p *Pipeline[T]) Submit(ctx context.Context, item T) error {
	p.submitted.Add(1)
	size := int64(p.cfg.Size(item))
	if size < 1 {
		size = 1
	}
	if size > p.cfg.QueueBytes {
		size = p.cfg.QueueBytes // one huge item takes the whole budget, not forever
	}

	p.closeMu.RLock()
	if p.closed {
		p.closeMu.RUnlock()
		return p.failItem(item, ErrClosed)
	}
	// Fast path: room now, no timer.
	if p.sem.TryAcquire(size) {
		select {
		case p.in <- queued[T]{item: item, size: size}:
			p.closeMu.RUnlock()
			return nil
		default:
		}
	} else {
		size = -size // not acquired yet
	}
	err := p.submitSlow(ctx, item, size)
	p.closeMu.RUnlock()
	return err
}

// SubmitWait queues item like Submit and then waits for its batch's outcome:
// nil once the sink took the batch, or the reason it did not. The record
// closes its batch at once, so it does not wait out the batch period. Both
// waits — for room and for the outcome — end with ctx (bound it); a wait
// that ends with ctx returns ctx's error and leaves the record in the
// pipeline, to be delivered or to take the failure path as any other.
func (p *Pipeline[T]) SubmitWait(ctx context.Context, item T) error {
	p.submitted.Add(1)
	size := int64(p.cfg.Size(item))
	if size < 1 {
		size = 1
	}
	if size > p.cfg.QueueBytes {
		size = p.cfg.QueueBytes
	}
	done := make(chan error, 1)
	p.closeMu.RLock()
	if p.closed {
		p.closeMu.RUnlock()
		return p.failItem(item, ErrClosed)
	}
	if err := p.sem.Acquire(ctx, size); err != nil {
		p.closeMu.RUnlock()
		return p.failItem(item, ErrQueueFull)
	}
	select {
	case p.in <- queued[T]{item: item, size: size, done: done}:
	case <-ctx.Done():
		p.sem.Release(size)
		p.closeMu.RUnlock()
		return p.failItem(item, ErrQueueFull)
	}
	p.closeMu.RUnlock()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TrySubmit queues item only if there is room now; otherwise it goes to
// Fail with ErrQueueFull at once (a marketing record set to drop).
func (p *Pipeline[T]) TrySubmit(item T) error {
	p.submitted.Add(1)
	size := int64(p.cfg.Size(item))
	if size < 1 {
		size = 1
	}
	if size > p.cfg.QueueBytes {
		size = p.cfg.QueueBytes
	}
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		return p.failItem(item, ErrClosed)
	}
	if !p.sem.TryAcquire(size) {
		return p.failItem(item, ErrQueueFull)
	}
	select {
	case p.in <- queued[T]{item: item, size: size}:
		return nil
	default:
		p.sem.Release(size)
		return p.failItem(item, ErrQueueFull)
	}
}

// submitSlow waits for room; size < 0 means its bytes are not yet acquired.
func (p *Pipeline[T]) submitSlow(ctx context.Context, item T, size int64) error {
	p.waited.Add(1)
	wctx, cancel := context.WithTimeout(ctx, p.cfg.BlockTimeout)
	defer cancel()
	if size < 0 {
		size = -size
		if err := p.sem.Acquire(wctx, size); err != nil {
			return p.failItem(item, ErrQueueFull)
		}
	}
	select {
	case p.in <- queued[T]{item: item, size: size}:
		return nil
	case <-wctx.Done():
		p.sem.Release(size)
		return p.failItem(item, ErrQueueFull)
	}
}

// failItem encodes one item that never reached a batch and fails it.
func (p *Pipeline[T]) failItem(item T, reason error) error {
	var rec []byte
	if b, err := p.encode(nil, item); err == nil {
		rec = b
	}
	f := Failure{Err: reason, Count: 1}
	if rec != nil {
		f.Records = [][]byte{rec}
	}
	p.fail(f)
	return reason
}

func (p *Pipeline[T]) encode(dst []byte, item T) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = dst, p.recovered("Encode", r)
		}
	}()
	return p.cfg.Encode(dst, item)
}

func (p *Pipeline[T]) tag(item T) (tag string) {
	defer func() {
		if r := recover(); r != nil {
			tag = ""
			_ = p.recovered("Tag", r)
		}
	}()
	return p.cfg.Tag(item)
}

func (p *Pipeline[T]) fail(f Failure) {
	p.failed.Add(uint64(f.Count))
	defer func() {
		if r := recover(); r != nil {
			_ = p.recovered("Fail", r)
		}
	}()
	p.cfg.Fail(f)
}

// batcher is stage 1: it encodes records into batches and hands each closed
// batch to the sender.
func (p *Pipeline[T]) batcher() {
	defer close(p.out)
	var (
		b       *Batch
		scratch []byte
		timer   = time.NewTimer(time.Hour)
		armed   bool
	)
	timer.Stop()
	flush := func() {
		if b == nil {
			return
		}
		if armed {
			timer.Stop()
			armed = false
		}
		if b.Len() == 0 {
			p.release(b)
		} else {
			p.out <- b
		}
		b = nil
	}
	for {
		select {
		case q, ok := <-p.in:
			if !ok {
				flush()
				return
			}
			rec, err := p.encode(scratch[:0], q.item)
			scratch = rec[:0]
			if err != nil {
				p.sem.Release(q.size)
				p.fail(Failure{Err: err, Count: 1})
				tell(q.done, err)
				continue
			}
			need := protowire.SizeTag(BatchFieldRecords) + protowire.SizeBytes(len(rec))
			if b != nil && len(b.Wire)+need > p.cfg.BatchBytes {
				flush()
			}
			if b == nil {
				b = p.newBatch()
				if len(b.Wire)+need > p.cfg.BatchBytes {
					p.sem.Release(q.size)
					p.fail(Failure{Err: ErrTooLarge, Records: [][]byte{rec}, Count: 1})
					tell(q.done, ErrTooLarge)
					continue
				}
				timer.Reset(p.cfg.BatchPeriod)
				armed = true
			}
			b.Wire = protowire.AppendTag(b.Wire, BatchFieldRecords, protowire.BytesType)
			b.Wire = protowire.AppendVarint(b.Wire, uint64(len(rec)))
			b.offsets = append(b.offsets, len(b.Wire))
			b.Wire = append(b.Wire, rec...)
			b.offsets = append(b.offsets, len(b.Wire))
			b.held += q.size
			if p.cfg.Tag != nil {
				b.addTag(p.tag(q.item))
			}
			if q.done != nil {
				// A waiting request: close the batch now rather than at the
				// period, which would only add to its wait.
				b.dones = append(b.dones, q.done)
				flush()
				continue
			}
			if b.Len() >= p.cfg.BatchItems {
				flush()
			}
		case <-timer.C:
			armed = false
			flush()
		}
	}
}

func (p *Pipeline[T]) newBatch() *Batch {
	buf := p.bufs.Get().(*[]byte)
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	b := &Batch{ID: id, Wire: (*buf)[:0]}
	b.Wire = protowire.AppendTag(b.Wire, BatchFieldID, protowire.BytesType)
	b.Wire = protowire.AppendBytes(b.Wire, id[:])
	return b
}

// release returns a batch's memory: its queue bytes and its buffer.
func (p *Pipeline[T]) release(b *Batch) {
	p.sem.Release(b.held)
	buf := b.Wire[:0]
	b.Wire = nil
	p.bufs.Put(&buf)
}

// sender is stage 2. A batch the sink refuses is tried again with backoff
// for RetryFor (the sink must be idempotent per batch id), then failed.
func (p *Pipeline[T]) sender() {
	defer close(p.done)
	for b := range p.out {
		var err error
		if p.aborted.Load() {
			err = ErrClosed
		} else {
			err = p.sinkRetrying(b)
		}
		if err == nil {
			p.delivered.Add(uint64(b.Len()))
			p.batches.Add(1)
		} else {
			recs := make([][]byte, b.Len())
			for i := range recs {
				recs[i] = b.Record(i)
			}
			p.fail(Failure{Err: err, BatchID: b.ID, Records: recs, Count: b.Len()})
		}
		for _, d := range b.dones {
			tell(d, err)
		}
		b.dones = nil
		p.release(b)
	}
}

func (p *Pipeline[T]) sinkRetrying(b *Batch) error {
	err := p.sink(b)
	if err == nil || p.cfg.RetryFor < 0 {
		return err
	}
	deadline := time.Now().Add(p.cfg.RetryFor)
	wait := 25 * time.Millisecond
	for err != nil && time.Now().Add(wait).Before(deadline) && !p.aborted.Load() {
		select {
		case <-p.sinkCtx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(wait*2, 500*time.Millisecond)
		err = p.sink(b)
	}
	return err
}

// tell hands a waiting SubmitWait its batch's outcome (never blocks: the
// channel holds one, and a caller that gave up no longer reads it).
func tell(done chan error, err error) {
	if done != nil {
		select {
		case done <- err:
		default:
		}
	}
}

func (p *Pipeline[T]) sink(b *Batch) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = p.recovered("Sink", r)
		}
	}()
	return p.cfg.Sink(p.sinkCtx, b)
}

// Close stops accepting records and drains both stages into the sink. When
// ctx ends first, the sink's context is cancelled and every record still
// held goes to Fail with ErrClosed; Close then returns ctx's error (a sink
// must therefore return once its context ends). Safe to call more than once.
func (p *Pipeline[T]) Close(ctx context.Context) error {
	p.closeMu.Lock()
	if !p.closed {
		p.closed = true
		close(p.in)
	}
	p.closeMu.Unlock()
	select {
	case <-p.done:
		p.cancelSink()
		return nil
	case <-ctx.Done():
		p.aborted.Store(true)
		p.cancelSink()
		<-p.done
		return ctx.Err()
	}
}

// Stats returns the counters.
func (p *Pipeline[T]) Stats() Stats {
	return Stats{
		Submitted: p.submitted.Load(), Delivered: p.delivered.Load(), Failed: p.failed.Load(),
		Batches: p.batches.Load(), Waited: p.waited.Load(), Panics: p.panics.Load(),
	}
}
