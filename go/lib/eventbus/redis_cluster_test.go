package eventbus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/redisdsn"
)

// clusterDSN is a cluster-mode Redis (`...?cluster=true`), set in CI's
// redis-cluster job; without it these tests skip.
func clusterDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("W17_TEST_REDIS_CLUSTER_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_REDIS_CLUSTER") != "" {
			t.Fatal("W17_REQUIRE_LIVE_REDIS_CLUSTER is set but W17_TEST_REDIS_CLUSTER_DSN is not")
		}
		t.Skip("W17_TEST_REDIS_CLUSTER_DSN is not set (a cluster-mode Redis, cluster=true)")
	}
	return dsn
}

// On a cluster every channel is delivered, whichever shard its stream lands
// on, and a failed delivery is claimed and offered again there — the paths
// a single-server client broke with MOVED on every shard but the one it
// dialled.
func TestRedisBus_Cluster_EveryChannelOnEveryShard(t *testing.T) {
	dsn := clusterDSN(t)
	run := fmt.Sprintf("c%d", time.Now().UnixNano())
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{
		DSN:               dsn,
		GroupPrefix:       run,
		DefaultAckWait:    500 * time.Millisecond,
		DefaultMaxDeliver: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })

	// Channels until three different shards hold one: the test is about
	// the shards the client did not dial.
	probe, err := redisdsn.Open(dsn, redisdsn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	cc, ok := probe.(*goredis.ClusterClient)
	if !ok {
		t.Fatalf("W17_TEST_REDIS_CLUSTER_DSN opens %T, want a cluster client (cluster=true)", probe)
	}
	shards := map[string]bool{}
	var channels []string
	for i := 0; len(shards) < 3 && i < 200; i++ {
		ch := fmt.Sprintf("%s-ch%d", run, i)
		node, err := cc.MasterForKey(context.Background(), ch)
		if err != nil {
			t.Fatal(err)
		}
		if addr := node.Options().Addr; !shards[addr] {
			shards[addr] = true
			channels = append(channels, ch)
		}
	}
	if len(shards) < 3 {
		t.Fatalf("the cluster has %d shards, want 3", len(shards))
	}

	var mu sync.Mutex
	got := map[string]int{}
	tries := map[string]int{}
	for _, ch := range channels {
		sub, err := bus.Subscriber(context.Background(), ch)
		if err != nil {
			t.Fatalf("Subscriber %s: %v", ch, err)
		}
		if err := sub.Subscribe(context.Background(), "**", func(_ context.Context, _ string, _ []byte) error {
			mu.Lock()
			defer mu.Unlock()
			tries[ch]++
			if ch == channels[len(channels)-1] && tries[ch] == 1 {
				return fmt.Errorf("the first delivery on %s fails", ch)
			}
			got[ch]++
			return nil
		}); err != nil {
			t.Fatalf("Subscribe %s: %v", ch, err)
		}
	}
	for _, ch := range channels {
		if err := bus.Dispatch(context.Background(), ch, "x.y", wrapperspb.String(ch)); err != nil {
			t.Fatalf("Dispatch %s: %v", ch, err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		done := len(got) == len(channels)
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("delivered %v of %v (tries %v)", got, channels, tries)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := channels[len(channels)-1]; tries[last] != 2 {
		t.Errorf("the failed delivery on %s was tried %d times, want 2", last, tries[last])
	}
}

// A single-server DSN at a cluster names the fix in its error.
func TestRedisBus_Cluster_ASingleServerDSNSaysToAddCluster(t *testing.T) {
	dsn := clusterDSN(t)
	single := dsn
	if i := len(single) - len("?cluster=true"); i > 0 && single[i:] == "?cluster=true" {
		single = single[:i]
	} else {
		t.Skipf("W17_TEST_REDIS_CLUSTER_DSN %q does not end in ?cluster=true", dsn)
	}
	bus, err := eventbus.NewRedisBus(eventbus.RedisBusOptions{DSN: single, GroupPrefix: "single"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(timeout(t, 5*time.Second)) })
	var refused error
	for i := 0; i < 20 && refused == nil; i++ {
		refused = bus.Dispatch(context.Background(), fmt.Sprintf("single-%d", i), "x.y", wrapperspb.String("p"))
	}
	if refused == nil {
		t.Fatal("20 channels all on the dialled shard: no MOVED to check")
	}
	if !errors.Is(refused, redisdsn.ErrClusterServer) {
		t.Errorf("Dispatch error %v does not say to add cluster=true", refused)
	}
}
