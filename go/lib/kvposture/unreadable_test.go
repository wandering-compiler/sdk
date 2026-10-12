package kvposture

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A server that will not say its eviction policy (a managed Redis with
// CONFIG disabled) gets a debug line and nothing louder: the check is
// advisory, and boot carries on at once.
func TestWarnIfEvicting_UnreadablePolicyLogsAtDebugOnly(t *testing.T) {
	srv := miniredis.RunT(t) // answers data commands, refuses CONFIG
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer func() { _ = client.Close() }()
	if _, err := client.ConfigGet(context.Background(), "maxmemory-policy").Result(); err == nil {
		t.Skip("this server answers CONFIG GET; the unreadable path is not reachable with it")
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	done := make(chan struct{})
	go func() {
		WarnIfEvicting(context.Background(), "catalog-redis", client)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the posture check held boot")
	}

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "not readable") {
		t.Errorf("want a debug line saying the policy is not readable; log = %q", out)
	}
	for _, loud := range []string{"level=WARN", "level=ERROR"} {
		if strings.Contains(out, loud) {
			t.Errorf("an unreadable policy logged at %s: %q", loud, out)
		}
	}
}
