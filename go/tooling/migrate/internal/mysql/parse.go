package mysql

import "fmt"

// Config is the parsed + validated mysql DSN, post URL→driver
// conversion. Holds both forms so callers + tests can inspect
// either.
type Config struct {
	// URLDSN is the input shape (`mysql://user:pass@host:port/db
	// ?param=value`) accepted by the w17ctl surface.
	URLDSN string

	// DriverDSN is the post-conversion form
	// (`user:pass@tcp(host:port)/db?multiStatements=true&…`)
	// passed to go-sql-driver/mysql via database/sql.
	DriverDSN string
}

// ParseDSN turns a `mysql://…` URL DSN into Config. Forces
// `multiStatements=true` on the resulting driver DSN so multi-
// statement DDL bodies the migrator emits go through in one
// Exec call.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, fmt.Errorf("mysql: dsn is empty")
	}
	driverDSN, err := URLToDriverDSN(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("mysql: %w", err)
	}
	return Config{URLDSN: dsn, DriverDSN: driverDSN}, nil
}

// Validate enforces invariants on a parsed Config. URLToDriverDSN
// already validates scheme + host shape; this layer just guards
// the empty-string corner.
func Validate(cfg Config) error {
	if cfg.URLDSN == "" || cfg.DriverDSN == "" {
		return fmt.Errorf("mysql: empty DSN")
	}
	return nil
}
