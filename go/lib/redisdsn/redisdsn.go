// Package redisdsn opens a Redis client from a DSN: one server, or a
// cluster.
//
// A DSN is `redis://` or `rediss://` with go-redis's URL options. One
// option is this package's: `cluster=true` names a cluster-mode server — a
// Redis Cluster, or ElastiCache / MemoryDB with cluster mode on — and the
// client then follows the cluster's slots, starting from the DSN's host (a
// configuration endpoint) and any `addr=` the DSN lists. Without it the
// client talks to the one server the DSN names; when that server is a
// cluster, every key another shard holds is refused with MOVED, and the
// error then says to add `cluster=true`.
//
// A cluster has database 0 only, so a cluster DSN names no other.
package redisdsn

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

// ClusterParam is the DSN option that names a cluster-mode server.
const ClusterParam = "cluster"

// Options tune the client beyond what the DSN says.
type Options struct {
	// ContextTimeout makes the client honour a context's deadline (go-redis
	// ignores it unless told).
	ContextTimeout bool
}

// IsCluster reports whether dsn names a cluster-mode server.
func IsCluster(dsn string) (bool, error) {
	_, cluster, err := split(dsn)
	return cluster, err
}

// Open returns the client dsn names: a cluster client with `cluster=true`,
// else a single-server client.
func Open(dsn string, o Options) (goredis.UniversalClient, error) {
	rest, cluster, err := split(dsn)
	if err != nil {
		return nil, err
	}
	if cluster {
		opt, err := goredis.ParseClusterURL(rest)
		if err != nil {
			return nil, fmt.Errorf("redis cluster DSN: %w", err)
		}
		opt.ContextTimeoutEnabled = o.ContextTimeout
		return goredis.NewClusterClient(opt), nil
	}
	opt, err := goredis.ParseURL(rest)
	if err != nil {
		return nil, fmt.Errorf("redis DSN: %w", err)
	}
	opt.ContextTimeoutEnabled = o.ContextTimeout
	c := goredis.NewClient(opt)
	c.AddHook(movedHint{})
	return c, nil
}

// split takes the cluster option out of dsn, which go-redis would refuse
// as unknown.
func split(dsn string) (string, bool, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", false, errors.New("redis DSN: not a URL")
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return "", false, fmt.Errorf("redis DSN: scheme %q, want redis or rediss", u.Scheme)
	}
	q := u.Query()
	raw, set := q[ClusterParam]
	if !set {
		return dsn, false, nil
	}
	if len(raw) != 1 {
		return "", false, fmt.Errorf("redis DSN: %s given %d times", ClusterParam, len(raw))
	}
	cluster, err := strconv.ParseBool(raw[0])
	if err != nil {
		return "", false, fmt.Errorf("redis DSN: %s=%q, want true or false", ClusterParam, raw[0])
	}
	q.Del(ClusterParam)
	u.RawQuery = q.Encode()
	if cluster {
		switch u.Path {
		case "", "/", "/0":
			u.Path = ""
		default:
			return "", false, fmt.Errorf("redis DSN: database %q — a cluster has database 0 only", strings.TrimPrefix(u.Path, "/"))
		}
	}
	return u.String(), cluster, nil
}

// movedHint turns a single-server client's MOVED reply — the server is a
// cluster and another shard holds the key — into an error that says what to
// change.
type movedHint struct{}

func (movedHint) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (movedHint) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if hinted, ok := hint(err); ok {
			cmd.SetErr(hinted)
			return hinted
		}
		return err
	}
}

func (movedHint) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			if hinted, ok := hint(cmd.Err()); ok {
				cmd.SetErr(hinted)
			}
		}
		if hinted, ok := hint(err); ok {
			return hinted
		}
		return err
	}
}

// ErrClusterServer marks a MOVED reply to a client that is not a cluster
// client.
var ErrClusterServer = errors.New("the Redis server runs in cluster mode: add " + ClusterParam + "=true to the DSN")

// hint wraps a MOVED reply in ErrClusterServer; ok is false (and err
// unchanged) for any other error, or one already wrapped.
func hint(err error) (hinted error, ok bool) {
	if err == nil || errors.Is(err, ErrClusterServer) || !strings.HasPrefix(err.Error(), "MOVED ") {
		return err, false
	}
	return fmt.Errorf("%w (%w)", ErrClusterServer, err), true
}
