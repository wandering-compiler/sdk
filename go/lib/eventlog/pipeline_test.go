package eventlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
)

// rec is the test's record: field 1 its number, field 2 a payload.
type rec struct {
	n       int
	payload string
	encErr  error
	panics  bool
}

func encodeRec(dst []byte, r *rec) ([]byte, error) {
	if r.panics {
		panic("encoder bug")
	}
	if r.encErr != nil {
		return dst, r.encErr
	}
	dst = protowire.AppendTag(dst, 1, protowire.VarintType)
	dst = protowire.AppendVarint(dst, uint64(r.n))
	dst = protowire.AppendTag(dst, 2, protowire.BytesType)
	return protowire.AppendString(dst, r.payload), nil
}

func recNumber(t testing.TB, b []byte) int {
	t.Helper()
	num, typ, n := protowire.ConsumeTag(b)
	if num != 1 || typ != protowire.VarintType || n < 0 {
		t.Fatalf("record does not start with field 1: %x", b)
	}
	v, _ := protowire.ConsumeVarint(b[n:])
	return int(v)
}

// decodeBatch reads a batch's wire form as a consumer would.
func decodeBatch(t testing.TB, wire []byte) (uuid.UUID, [][]byte) {
	t.Helper()
	var id uuid.UUID
	var recs [][]byte
	for len(wire) > 0 {
		num, typ, n := protowire.ConsumeTag(wire)
		if n < 0 || typ != protowire.BytesType {
			t.Fatalf("bad batch tag")
		}
		wire = wire[n:]
		v, n := protowire.ConsumeBytes(wire)
		if n < 0 {
			t.Fatalf("bad batch field")
		}
		wire = wire[n:]
		switch num {
		case BatchFieldID:
			copy(id[:], v)
		case BatchFieldRecords:
			recs = append(recs, v)
		}
	}
	return id, recs
}

type harness struct {
	t     *testing.T
	mu    sync.Mutex
	got   []int // delivered record numbers, in order
	sizes []int // delivered batch sizes in bytes
	lens  []int // records per batch
	ids   map[uuid.UUID]bool
	fails []Failure
	gate  chan struct{} // when non-nil, the sink waits on it
	sinkE error
}

func newHarness(t *testing.T) *harness { return &harness{t: t, ids: map[uuid.UUID]bool{}} }

func (h *harness) config() Config[*rec] {
	return Config[*rec]{
		Size:   func(r *rec) int { return 16 + len(r.payload) },
		Encode: encodeRec,
		Sink: func(ctx context.Context, b *Batch) error {
			if h.gate != nil {
				select {
				case <-h.gate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if h.sinkE != nil {
				return h.sinkE
			}
			id, recs := decodeBatch(h.t, b.Wire)
			if id != b.ID || len(recs) != b.Len() {
				h.t.Errorf("batch wire disagrees with the batch: id %v/%v, %d/%d records", id, b.ID, len(recs), b.Len())
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.ids[id] {
				h.t.Errorf("batch id %v reused", id)
			}
			h.ids[id] = true
			for i, r := range recs {
				if string(r) != string(b.Record(i)) {
					h.t.Errorf("Record(%d) differs from the wire", i)
				}
				h.got = append(h.got, recNumber(h.t, r))
			}
			h.sizes = append(h.sizes, len(b.Wire))
			h.lens = append(h.lens, b.Len())
			return nil
		},
		Fail: func(f Failure) {
			h.mu.Lock()
			defer h.mu.Unlock()
			cp := f
			cp.Records = nil
			for _, r := range f.Records {
				cp.Records = append(cp.Records, append([]byte(nil), r...))
			}
			h.fails = append(h.fails, cp)
		},
		BatchBytes: 1 << 20,
	}
}

func (h *harness) failed() []Failure {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Failure(nil), h.fails...)
}

func (h *harness) delivered() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.got...)
}

func start(t *testing.T, cfg Config[*rec]) *Pipeline[*rec] {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Close(ctx)
	})
	return p
}

func closeNow(t *testing.T, p *Pipeline[*rec]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPipeline_DeliversEveryRecordInOrder(t *testing.T) {
	h := newHarness(t)
	p := start(t, h.config())
	for i := 0; i < 5000; i++ {
		if err := p.Submit(context.Background(), &rec{n: i, payload: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	closeNow(t, p)
	got := h.delivered()
	if len(got) != 5000 {
		t.Fatalf("delivered %d of 5000", len(got))
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("record %d delivered as %d: order lost", i, n)
		}
	}
	if s := p.Stats(); s.Submitted != 5000 || s.Delivered != 5000 || s.Failed != 0 {
		t.Fatalf("stats %+v", s)
	}
	if len(h.failed()) != 0 {
		t.Fatalf("failures: %v", h.failed())
	}
}

func TestPipeline_BatchClosesByCount(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchItems, cfg.BatchPeriod = 100, time.Hour
	p := start(t, cfg)
	for i := 0; i < 350; i++ {
		_ = p.Submit(context.Background(), &rec{n: i})
	}
	closeNow(t, p)
	if fmt.Sprint(h.lens) != "[100 100 100 50]" {
		t.Fatalf("batch lengths %v", h.lens)
	}
}

func TestPipeline_BatchClosesByPeriod(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchItems, cfg.BatchPeriod = 1_000_000, 20*time.Millisecond
	p := start(t, cfg)
	_ = p.Submit(context.Background(), &rec{n: 1})
	deadline := time.Now().Add(2 * time.Second)
	for len(h.delivered()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a lone record was never delivered by the period")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPipeline_BatchClosesBySize(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchBytes, cfg.BatchPeriod = 4096, time.Hour
	p := start(t, cfg)
	for i := 0; i < 200; i++ {
		_ = p.Submit(context.Background(), &rec{n: i, payload: string(make([]byte, 100))})
	}
	closeNow(t, p)
	if len(h.delivered()) != 200 {
		t.Fatalf("delivered %d", len(h.delivered()))
	}
	for _, s := range h.sizes {
		if s > 4096 {
			t.Fatalf("a batch of %d bytes, limit 4096", s)
		}
	}
	if len(h.sizes) < 5 {
		t.Fatalf("only %d batches: the size bound did not close them", len(h.sizes))
	}
}

// A slow sink slows the producers down; nothing is lost.
func TestPipeline_BackpressureLosesNothing(t *testing.T) {
	h := newHarness(t)
	h.gate = make(chan struct{})
	cfg := h.config()
	cfg.QueueBytes, cfg.BatchItems, cfg.SendQueue, cfg.BlockTimeout = 2000, 10, 1, 10*time.Second
	p := start(t, cfg)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = p.Submit(context.Background(), &rec{n: w*1000 + i, payload: "p"})
			}
		}(w)
	}
	time.Sleep(50 * time.Millisecond) // producers are blocked now
	if p.Stats().Waited == 0 {
		t.Fatal("no producer waited: the queue bound did not hold")
	}
	go func() {
		for {
			select {
			case h.gate <- struct{}{}:
			case <-time.After(3 * time.Second):
				return
			}
		}
	}()
	wg.Wait()
	closeNow(t, p)
	if n := len(h.delivered()); n != 1600 || len(h.failed()) != 0 {
		t.Fatalf("delivered %d of 1600, %d failures", n, len(h.failed()))
	}
}

// The queue holds at most QueueBytes of records: with the sink stuck, the
// records beyond it take the failure path once the wait runs out.
func TestPipeline_QueueBoundInBytes(t *testing.T) {
	h := newHarness(t)
	h.gate = make(chan struct{})
	cfg := h.config()
	cfg.QueueBytes, cfg.BlockTimeout = 1000, 20*time.Millisecond // a record holds 100
	p := start(t, cfg)
	accepted := 0
	for i := 0; i < 30; i++ {
		if p.Submit(context.Background(), &rec{n: i, payload: string(make([]byte, 84))}) == nil {
			accepted++
		}
	}
	if accepted != 10 {
		t.Fatalf("accepted %d records of 100 bytes into a 1000-byte queue", accepted)
	}
	f := h.failed()
	if len(f) != 20 || !errors.Is(f[0].Err, ErrQueueFull) || len(f[0].Records) != 1 || recNumber(t, f[0].Records[0]) != 10 {
		t.Fatalf("failures: %d, first %+v", len(f), f)
	}
	close(h.gate)
}

func TestPipeline_WaitEndsWithTheRequest(t *testing.T) {
	h := newHarness(t)
	h.gate = make(chan struct{})
	cfg := h.config()
	cfg.QueueBytes, cfg.BlockTimeout = 16, time.Hour
	p := start(t, cfg)
	_ = p.Submit(context.Background(), &rec{n: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if err := p.Submit(ctx, &rec{n: 1}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Submit waited %v past its request's end", d)
	}
	close(h.gate)
}

func TestPipeline_FailurePaths(t *testing.T) {
	h := newHarness(t)
	p := start(t, h.config())
	_ = p.Submit(context.Background(), &rec{n: 1, encErr: errors.New("bad record")})
	_ = p.Submit(context.Background(), &rec{n: 2, panics: true})
	_ = p.Submit(context.Background(), &rec{n: 3})
	closeNow(t, p)
	f := h.failed()
	if len(f) != 2 || f[0].Err.Error() != "bad record" || !errors.Is(f[1].Err, ErrPanic) {
		t.Fatalf("failures %+v", f)
	}
	if fmt.Sprint(h.delivered()) != "[3]" {
		t.Fatalf("the pipeline did not go on after an encoder panic: %v", h.delivered())
	}
	if s := p.Stats(); s.Panics != 1 || s.Failed != 2 || s.Delivered != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestPipeline_SinkErrorAndPanicFailTheBatch(t *testing.T) {
	for _, c := range []struct {
		name string
		sink func(context.Context, *Batch) error
		want error
	}{
		{"error", func(context.Context, *Batch) error { return errors.New("nats down") }, nil},
		{"panic", func(context.Context, *Batch) error { panic("sink bug") }, ErrPanic},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.config()
			cfg.Sink = c.sink
			var calls atomic.Int32
			inner := cfg.Sink
			cfg.Sink = func(ctx context.Context, b *Batch) error { calls.Add(1); return inner(ctx, b) }
			p := start(t, cfg)
			for i := 0; i < 3; i++ {
				_ = p.Submit(context.Background(), &rec{n: i})
			}
			closeNow(t, p)
			f := h.failed()
			if len(f) != 1 || f[0].Count != 3 || len(f[0].Records) != 3 || f[0].BatchID == (uuid.UUID{}) {
				t.Fatalf("failures %+v", f)
			}
			if c.want != nil && !errors.Is(f[0].Err, c.want) {
				t.Fatalf("err %v", f[0].Err)
			}
			if recNumber(t, f[0].Records[2]) != 2 {
				t.Fatal("failed records are not the batch's")
			}
		})
	}
}

func TestPipeline_TooLargeRecord(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchBytes = 256
	p := start(t, cfg)
	_ = p.Submit(context.Background(), &rec{n: 1, payload: string(make([]byte, 300))})
	_ = p.Submit(context.Background(), &rec{n: 2})
	closeNow(t, p)
	if f := h.failed(); len(f) != 1 || !errors.Is(f[0].Err, ErrTooLarge) {
		t.Fatalf("failures %+v", f)
	}
	if fmt.Sprint(h.delivered()) != "[2]" {
		t.Fatalf("delivered %v", h.delivered())
	}
}

func TestPipeline_CloseDrainsThenRefuses(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchPeriod = time.Hour // only Close can flush the last batch
	p := start(t, cfg)
	for i := 0; i < 10; i++ {
		_ = p.Submit(context.Background(), &rec{n: i})
	}
	closeNow(t, p)
	if len(h.delivered()) != 10 {
		t.Fatalf("Close delivered %d of 10", len(h.delivered()))
	}
	if err := p.Submit(context.Background(), &rec{n: 99}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close: %v", err)
	}
	if f := h.failed(); len(f) != 1 || recNumber(t, f[0].Records[0]) != 99 {
		t.Fatalf("the refused record was not failed: %+v", f)
	}
	closeNow(t, p) // twice is fine
}

// A drain that runs out of time fails what it still holds instead of
// waiting for a stuck sink forever.
func TestPipeline_CloseBoundedByItsContext(t *testing.T) {
	h := newHarness(t)
	h.gate = make(chan struct{}) // the sink never proceeds
	cfg := h.config()
	cfg.BatchItems, cfg.SendQueue = 5, 1
	p := start(t, cfg)
	for i := 0; i < 50; i++ {
		_ = p.Submit(context.Background(), &rec{n: i})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v", err)
	}
	failed := 0
	for _, f := range h.failed() {
		failed += f.Count
	}
	if failed+len(h.delivered()) != 50 {
		t.Fatalf("%d failed + %d delivered of 50: records lost unreported", failed, len(h.delivered()))
	}
}

// Submit racing Close never panics (a send on a closed queue) and every
// record is accounted for.
func TestPipeline_SubmitRacingClose(t *testing.T) {
	for round := 0; round < 20; round++ {
		h := newHarness(t)
		p := start(t, h.config())
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					_ = p.Submit(context.Background(), &rec{n: i})
				}
			}()
		}
		time.Sleep(time.Duration(round) * 100 * time.Microsecond)
		closeNow(t, p)
		wg.Wait()
		failed := 0
		for _, f := range h.failed() {
			failed += f.Count
		}
		if failed+len(h.delivered()) != 1600 {
			t.Fatalf("round %d: %d + %d of 1600", round, failed, len(h.delivered()))
		}
	}
}

func TestPipeline_SubmitDoesNotAllocate(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	h := newHarness(t)
	p := start(t, h.config())
	r := &rec{n: 1, payload: "x"}
	allocs := testing.AllocsPerRun(1000, func() { _ = p.Submit(context.Background(), r) })
	if allocs != 0 {
		t.Fatalf("Submit allocates %v times", allocs)
	}
}

func TestNew_RequiresItsParts(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Sink = nil
	if _, err := New(cfg); err == nil {
		t.Fatal("no sink accepted")
	}
	cfg = h.config()
	cfg.BatchBytes = 0
	if _, err := New(cfg); err == nil {
		t.Fatal("no batch bound accepted")
	}
}

func BenchmarkSubmit(b *testing.B) {
	p, _ := New(Config[*rec]{
		Size: func(r *rec) int { return 64 }, Encode: encodeRec,
		Sink: func(context.Context, *Batch) error { return nil }, Fail: func(Failure) {},
		BatchBytes: 1 << 20,
	})
	defer func() { _ = p.Close(context.Background()) }()
	r := &rec{n: 1, payload: "0123456789abcdef0123456789abcdef"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.Submit(context.Background(), r)
		}
	})
}

func TestPipeline_BatchCarriesItsDistinctTags(t *testing.T) {
	var mu sync.Mutex
	var tags [][]string
	var overflow []bool
	cfg := newHarness(t).config()
	cfg.BatchItems, cfg.BatchPeriod = 100, time.Hour
	cfg.Tag = func(r *rec) string { return r.payload }
	cfg.Sink = func(_ context.Context, b *Batch) error {
		mu.Lock()
		defer mu.Unlock()
		tags = append(tags, append([]string(nil), b.Tags...))
		overflow = append(overflow, b.TagsOverflow)
		return nil
	}
	p := start(t, cfg)
	for i := 0; i < 100; i++ { // one batch: org-a, org-b, an untagged one
		_ = p.Submit(context.Background(), &rec{n: i, payload: []string{"org-a", "org-b", ""}[i%3]})
	}
	for i := 0; i < 100; i++ { // one batch: 100 distinct tags
		_ = p.Submit(context.Background(), &rec{n: i, payload: fmt.Sprintf("org-%d", i)})
	}
	closeNow(t, p)
	if fmt.Sprint(tags) != "[[org-a org-b] []]" || fmt.Sprint(overflow) != "[false true]" {
		t.Fatalf("tags %v overflow %v", tags, overflow)
	}
}
