package eventlog

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// The gateway's recorder and the subscriber's request consumers each pick the
// request stream's transport from their own environment (NATS when its DSN is
// set, else Redis). Set apart — a gateway with only the Redis DSN, a
// subscriber with the NATS one — both boot green and the consumers read an
// empty stream forever. Each side now says at boot which stream it uses, so
// the split shows in the two logs side by side.
func TestRequestStream_EachSideSaysWhichTransportItUses(t *testing.T) {
	mr, _ := redisFixture(t)
	dsn := "redis://" + mr.Addr()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	r, err := Open(context.Background(), dsn, RecorderConfig{Domain: "tasks"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = r.Close(context.Background())

	cs := &Consumers{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = cs.Serve(ctx, dsn, ConsumerConfig{DurablePrefix: "said"})

	got := buf.String()
	for _, want := range []string{
		"eventlog: recording requests to the Redis Streams request stream at redis://" + mr.Addr(),
		"eventlog: request consumers read the Redis Streams request stream at redis://" + mr.Addr(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}
