package eventlog

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// On a cluster-mode Redis (W17_TEST_REDIS_CLUSTER_DSN, cluster=true, set in
// CI's redis-cluster job) the request stream works end to end from the DSN:
// the recorder stores through the publish script, and a consumer's group
// reads it back. The keys share the {w17:requests} hash tag, so whichever
// shard holds the slot serves every command.
func TestRedisDSN_Cluster_OpenAndServe(t *testing.T) {
	dsn := os.Getenv("W17_TEST_REDIS_CLUSTER_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_REDIS_CLUSTER") != "" {
			t.Fatal("W17_REQUIRE_LIVE_REDIS_CLUSTER is set but W17_TEST_REDIS_CLUSTER_DSN is not")
		}
		t.Skip("W17_TEST_REDIS_CLUSTER_DSN is not set (a cluster-mode Redis, cluster=true)")
	}
	nonce := fmt.Sprintf("cluster-%d", time.Now().UnixNano())
	r, err := Open(context.Background(), dsn, RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rec := r.Surface(w17pb.RequestApi_REQUEST_API_REST).Start(healthMethod)
		rec.SetRequest(&healthpb.HealthCheckRequest{Service: nonce})
		rec.Finish(context.Background(), 200)
	}
	closeRecorder(t, r)

	var got atomic.Int32
	cs := &Consumers{}
	_ = cs.Audit("a", funcConsumer{audit: func(_ context.Context, b *ConsumedBatch) error {
		for _, rec := range b.Records {
			if req, ok := rec.Request.(*healthpb.HealthCheckRequest); ok && req.GetService() == nonce {
				got.Add(1)
			}
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cs.Serve(ctx, dsn, ConsumerConfig{DurablePrefix: nonce}) }()
	waitUntil(t, func() bool { return got.Load() == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
