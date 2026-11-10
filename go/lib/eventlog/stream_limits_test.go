package eventlog

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// A consumer process that starts before any recorder creates the request
// stream with the limits it was given for it, not the package defaults: the
// first binary to ensure the stream decides its limits for good.
func TestConsumers_CreateTheRequestStreamWithTheConfiguredLimits(t *testing.T) {
	_, js := embeddedJetStream(t)
	cs := &Consumers{}
	_ = cs.Audit("a", funcConsumer{audit: func(context.Context, *ConsumedBatch) error { return nil }})
	stop := runConsumers(t, cs, js, ConsumerConfig{RequestStream: StreamLimits{MaxAge: time.Hour, MaxBytes: MinStreamBytes}})
	defer stop()
	var cfg jetstream.StreamConfig
	waitUntil(t, func() bool {
		s, err := js.Stream(context.Background(), StreamName)
		if err != nil {
			return false
		}
		cfg = s.CachedInfo().Config
		return true
	})
	if cfg.MaxAge != time.Hour || cfg.MaxBytes != MinStreamBytes {
		t.Fatalf("W17_REQUESTS created with max_age %v, max_bytes %d; want the consumer's configured 1h, %d", cfg.MaxAge, cfg.MaxBytes, MinStreamBytes)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// An existing stream keeps its own limits; a binary that asked for others is
// told so, once per ensure, naming what differs.
func TestEnsureStream_SaysWhenAnExistingStreamsLimitsDiffer(t *testing.T) {
	_, js := embeddedJetStream(t)
	ctx := context.Background()
	if err := EnsureStream(ctx, js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{MaxAge: 2 * time.Hour, MaxBytes: MinStreamBytes}); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)
	if err := EnsureStream(ctx, js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "max_age") {
		t.Fatalf("no line says the existing stream's max_age differs from the configured one: %q", buf.String())
	}
	buf.Reset()
	if err := EnsureStream(ctx, js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{MaxAge: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("matching limits were reported: %q", buf.String())
	}
}

// A stream capped below what was asked (the server had no room) is reported
// as an error, not only logged: the cap stays until the stream is edited.
func TestEnsureStream_AHalvedCapIsReported(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.JetStreamMaxStore = 256 << 20
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, _ := jetstream.New(nc)
	buf := captureLog(t)
	if err := EnsureStream(context.Background(), js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{MaxBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "error:") || !strings.Contains(buf.String(), "capped") {
		t.Fatalf("the halved cap was not reported as an error: %q", buf.String())
	}
}
