package postgres

import (
	"fmt"
	"strings"
)

// Config is the parsed + validated postgres DSN. pgx accepts the
// DSN directly so Config is a thin wrapper, but the per-dialect
// type lives here per the D-iter3-10 contract (ParseDSN +
// Validate uniform across every apply/<dialect>/).
type Config struct {
	// DSN is the URL-form (`postgres://…`) or keyword-form (`host=
	// … user=… …`) connection string. Passed verbatim to
	// pgx.Connect.
	DSN string
}

// ParseDSN turns a postgres connection string into Config. The
// scheme check is intentionally permissive — pgx accepts both
// URL form (`postgres://…`, `postgresql://…`) and the keyword
// form (`host=… user=…`). Anything non-empty is acceptable here;
// pgx surfaces shape errors at Connect time.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, fmt.Errorf("postgres: dsn is empty")
	}
	if u := strings.TrimSpace(dsn); u == "" {
		return Config{}, fmt.Errorf("postgres: dsn is whitespace")
	}
	return Config{DSN: dsn}, nil
}

// Validate enforces invariants on a parsed Config beyond what
// ParseDSN already checked. For postgres the DSN's structural
// shape is pgx's responsibility; we only re-check the not-empty
// invariant here.
func Validate(cfg Config) error {
	if cfg.DSN == "" {
		return fmt.Errorf("postgres: empty DSN")
	}
	return nil
}
