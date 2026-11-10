package eventlog

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The organizations header names a batch's organizations so a consumer can
// skip a batch without decoding it. When it cannot name them exactly — too
// long for the room headers have, or an id that holds the separator — it
// must say "*" (decode to know), never a list a consumer would misread.
func TestBatchOrgs_SaysStarWhenItCannotNameThemExactly(t *testing.T) {
	long := strings.Repeat("o", 300)
	for name, tc := range map[string]struct {
		b    Batch
		want string
	}{
		"none":         {Batch{}, ""},
		"two":          {Batch{Tags: []string{"org-1", "org-2"}}, "org-1,org-2"},
		"overflow":     {Batch{TagsOverflow: true}, "*"},
		"a comma":      {Batch{Tags: []string{"org-1", "a,b"}}, "*"},
		"a star":       {Batch{Tags: []string{"*"}}, "*"},
		"too long":     {Batch{Tags: []string{long, long + "1", long + "2", long + "3", long + "4", long + "5", long + "6", long + "7"}}, "*"},
		"just fitting": {Batch{Tags: []string{long, long + "1"}}, long + "," + long + "1"},
	} {
		if got := batchOrgs(&tc.b); got != tc.want {
			t.Errorf("%s: batchOrgs = %.40q…, want %.40q…", name, got, tc.want)
		}
	}
}

// Both sinks carry what batchOrgs says.
func TestSinks_CarryTheBoundedOrgs(t *testing.T) {
	b := &Batch{ID: uuid.New(), Wire: []byte("x"), Tags: []string{"org-1", "a,b"}}

	_, c := redisFixture(t)
	if err := (&RedisSink{Client: c, Subject: "w17.requests.tasks.rest"}).Sink(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	entries, err := c.XRange(context.Background(), RedisStreamKey, "-", "+").Result()
	if err != nil || len(entries) != 1 || entries[0].Values[redisFieldOrgs] != "*" {
		t.Fatalf("redis entry orgs = %v (%v), want *", entries, err)
	}

	_, js := embeddedJetStream(t)
	if err := EnsureStream(context.Background(), js, StreamName, []string{SubjectPrefix + ".>"}, StreamLimits{MaxBytes: MinStreamBytes}); err != nil {
		t.Fatal(err)
	}
	if err := (&NATSSink{JS: js, Subject: "w17.requests.tasks.rest"}).Sink(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	st, _ := js.Stream(context.Background(), StreamName)
	msg, err := st.GetLastMsgForSubject(context.Background(), "w17.requests.tasks.rest")
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get(HeaderOrgs); got != "*" {
		t.Fatalf("NATS header %s = %q, want *", HeaderOrgs, got)
	}
}
