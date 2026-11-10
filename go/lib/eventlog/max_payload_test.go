package eventlog

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// A recorder opened before NATS is up cannot read the server's max_payload
// (0 until the server says), so it starts with NATS's default. Once the
// server is up its own, smaller limit must bound the batches: a burst of
// records larger than it together is split, not refused as one oversized
// publish.
func TestOpen_BatchesFollowTheServersMaxPayloadOnceConnected(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	var mu sync.Mutex
	var fails []Failure
	r, err := Open(context.Background(), "nats://127.0.0.1:"+strconv.Itoa(port), RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
		Fail: func(f Failure) {
			mu.Lock()
			defer mu.Unlock()
			fails = append(fails, f)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	opts := natsserver.DefaultTestOptions
	opts.Port = port
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.JetStreamMaxStore = 1 << 30
	opts.MaxPayload = 64 << 10
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	deadline := time.Now().Add(10 * time.Second)
	for !r.conn.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !r.conn.IsConnected() {
		t.Fatal("the recorder never connected")
	}

	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	record := func(n int) {
		rec := s.Start(testMethod)
		rec.SetRequest(&descriptorpb.FieldDescriptorProto{Name: proto.String(strings.Repeat("x", n))})
		rec.Finish(context.Background(), 200)
	}
	record(10)
	time.Sleep(200 * time.Millisecond)
	for range 5 {
		record(20 << 10) // 5 × 20 KiB: one batch would be ~100 KiB
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fails) != 0 {
		t.Fatalf("%d failures, first %v: a batch was built past the server's max_payload", len(fails), fails[0].Err)
	}
}
