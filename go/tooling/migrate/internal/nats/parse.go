package nats

import (
	"fmt"
	"net/url"
)

// Config is the parsed + validated nats DSN. Today this is the
// `<scheme>://<host>:<port>` shape the nats.go client's
// `nats.Connect` consumes; future TLS / auth options layer on top.
type Config struct {
	// DSN is the input shape (`nats://[user:pass@]host[:port]`).
	DSN string

	// Server is the value `nats.Connect` consumes. User-info is
	// dropped at parse time (auth lands when Phase G ships).
	Server string
}

// ParseDSN turns a nats URL DSN into Config.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, fmt.Errorf("nats: dsn is empty")
	}
	server, err := DSNToServer(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("nats: %w", err)
	}
	return Config{DSN: dsn, Server: server}, nil
}

// Validate enforces invariants on a parsed Config.
func Validate(cfg Config) error {
	if cfg.Server == "" {
		return fmt.Errorf("nats: empty Server")
	}
	return nil
}

// DSNToServer extracts the `<scheme>://<host>:<port>` shape the
// nats.go `Connect` accepts. Drops user info + path + query
// (nats.Connect uses option funcs for credentials when
// authenticated; w17ctl keeps this lean — secret-bearing
// auth comes in Phase G alongside console-side auth).
func DSNToServer(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "nats" {
		return "", fmt.Errorf("expected nats:// scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	return "nats://" + u.Host, nil
}
