package redisdsn

import (
	"errors"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestSplit(t *testing.T) {
	for _, c := range []struct {
		dsn, rest string
		cluster   bool
		err       string
	}{
		{dsn: "redis://bus:6379/0", rest: "redis://bus:6379/0"},
		{dsn: "rediss://u:p@bus:6379/2?dial_timeout=3", rest: "rediss://u:p@bus:6379/2?dial_timeout=3"},
		{dsn: "rediss://u:p@cfg.example:6379?cluster=true", rest: "rediss://u:p@cfg.example:6379", cluster: true},
		{dsn: "redis://a:7000/0?cluster=1&addr=b:7001", rest: "redis://a:7000?addr=b%3A7001", cluster: true},
		{dsn: "redis://a:7000/?cluster=false", rest: "redis://a:7000/"},
		{dsn: "redis://a:7000/3?cluster=true", err: "database 0 only"},
		{dsn: "redis://a:7000?cluster=yes", err: "want true or false"},
		{dsn: "redis://a:7000?cluster=true&cluster=true", err: "2 times"},
		{dsn: "nats://a:4222", err: "scheme"},
	} {
		rest, cluster, err := split(c.dsn)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("split(%q) err %v, want %q", c.dsn, err, c.err)
			}
			continue
		}
		if err != nil || rest != c.rest || cluster != c.cluster {
			t.Errorf("split(%q) = %q, %v, %v; want %q, %v", c.dsn, rest, cluster, err, c.rest, c.cluster)
		}
	}
}

func TestOpen_PicksTheClientTheDSNNames(t *testing.T) {
	c, err := Open("redis://127.0.0.1:1/0?cluster=true", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(*goredis.ClusterClient); !ok {
		t.Errorf("cluster DSN gave %T", c)
	}
	_ = c.Close()
	c, err = Open("redis://127.0.0.1:1/0", Options{ContextTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := c.(*goredis.Client); !ok || !s.Options().ContextTimeoutEnabled {
		t.Errorf("single DSN gave %T", c)
	}
	_ = c.Close()
	if _, err := Open("redis://127.0.0.1:1/0?cluster=maybe", Options{}); err == nil {
		t.Error("a bad cluster option opened")
	}
}

func TestHint_SaysToAddClusterOnMOVEDOnly(t *testing.T) {
	moved := errors.New("MOVED 12682 10.0.0.3:6379")
	got, ok := hint(moved)
	if !ok || !errors.Is(got, ErrClusterServer) || !errors.Is(got, moved) || !strings.Contains(got.Error(), "cluster=true") {
		t.Errorf("hint(MOVED) = %v, %v", got, ok)
	}
	if _, again := hint(got); again {
		t.Error("hinted twice")
	}
	for _, e := range []error{nil, goredis.Nil, errors.New("NOGROUP no such key")} {
		if _, changed := hint(e); changed {
			t.Errorf("hint(%v) changed it", e)
		}
	}
}
