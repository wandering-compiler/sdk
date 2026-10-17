package eventlog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func embeddedJetStream(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return nc, js
}

// End to end: records through the pipeline into JetStream; every batch
// stored once (a re-sent id is dropped), labelled with its organizations,
// decoding back to its records.
func TestNATSSink_StoresEachBatchOnce(t *testing.T) {
	nc, js := embeddedJetStream(t)
	ctx := context.Background()
	if err := EnsureStream(ctx, js, "W17_REQUESTS", []string{"reqs.>"}, StreamLimits{}); err != nil {
		t.Fatal(err)
	}
	sink := &NATSSink{JS: js, Subject: "reqs.tasks.rest"}
	h := newHarness(t)
	cfg := h.config()
	cfg.BatchBytes, cfg.BatchItems, cfg.BatchPeriod = NATSBatchBytes(nc), 100, time.Hour
	cfg.Tag = func(r *rec) string { return r.payload }
	var resent bool
	cfg.Sink = func(ctx context.Context, b *Batch) error {
		if err := sink.Sink(ctx, b); err != nil {
			return err
		}
		if !resent { // a re-send after a lost ack
			resent = true
			return sink.Sink(ctx, b)
		}
		return nil
	}
	p := start(t, cfg)
	for i := 0; i < 250; i++ {
		_ = p.Submit(ctx, &rec{n: i, payload: fmt.Sprintf("org-%d", i%2)})
	}
	closeNow(t, p)
	if len(h.failed()) != 0 {
		t.Fatalf("failures %+v", h.failed())
	}

	stream, _ := js.Stream(ctx, "W17_REQUESTS")
	info, _ := stream.Info(ctx)
	if info.State.Msgs != 3 {
		t.Fatalf("stream holds %d messages, want 3 batches (the re-send dropped)", info.State.Msgs)
	}
	seen := 0
	for seq := uint64(1); seq <= info.State.LastSeq; seq++ {
		m, err := stream.GetMsg(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		if m.Header.Get(HeaderOrgs) != "org-0,org-1" {
			t.Errorf("batch %d orgs header %q", seq, m.Header.Get(HeaderOrgs))
		}
		id, recs := decodeBatch(t, m.Data)
		if m.Header.Get(jetstream.MsgIDHeader) != id.String() {
			t.Errorf("message id %q is not the batch id %s", m.Header.Get(jetstream.MsgIDHeader), id)
		}
		for _, r := range recs {
			if recNumber(t, r) != seen {
				t.Fatalf("record %d out of order", seen)
			}
			seen++
		}
	}
	if seen != 250 {
		t.Fatalf("stored %d records of 250", seen)
	}
}

// No stream over the subject: the publish is not acknowledged, and the
// batch takes the failure path instead of counting as delivered.
func TestNATSSink_UnstoredBatchFails(t *testing.T) {
	_, js := embeddedJetStream(t)
	sink := &NATSSink{JS: js, Subject: "nowhere.x", Timeout: 500 * time.Millisecond}
	b := &Batch{Wire: []byte{0x0a, 0x00}}
	if err := sink.Sink(context.Background(), b); err == nil {
		t.Fatal("a publish no stream stored was reported delivered")
	}
}

func TestNATSSink_HonoursItsContext(t *testing.T) {
	_, js := embeddedJetStream(t)
	sink := &NATSSink{JS: js, Subject: "nowhere.x", Timeout: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Now()
	err := sink.Sink(ctx, &Batch{Wire: []byte{0x0a, 0x00}})
	if err == nil || time.Since(t0) > time.Second || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v after %v", err, time.Since(t0))
	}
}
