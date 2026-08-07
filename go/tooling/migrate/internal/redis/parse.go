package redis

import (
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// Config is the parsed + validated redis DSN, expressed as the
// go-redis `Options` the typed client accepts. Tests can inspect
// Options directly without re-deriving from a string.
type Config struct {
	// DSN is the input shape (`redis://[user:pass@]host[:port]
	// [/db]` or `rediss://…` for TLS).
	DSN string

	// Options is the go-redis options shape (Addr, Username,
	// Password, DB, TLSConfig). Built from DSN via
	// `goredis.ParseURL`.
	Options *goredis.Options
}

// ParseDSN turns a redis URL DSN into Config.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, fmt.Errorf("redis: dsn is empty")
	}
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("redis: %w", err)
	}
	return Config{DSN: dsn, Options: opts}, nil
}

// Validate enforces invariants on a parsed Config. ParseURL
// already validates scheme + host; this layer just guards the
// empty-string corner.
func Validate(cfg Config) error {
	if cfg.DSN == "" || cfg.Options == nil {
		return fmt.Errorf("redis: empty DSN")
	}
	return nil
}
