package sqlite

import "fmt"

// Config is the parsed + validated sqlite DSN. Carries the
// post-strip path that modernc.org/sqlite expects.
type Config struct {
	// URLDSN is the input shape (`sqlite:///abs/path.db`,
	// `sqlite://relative.db`, or driver-native `file:…`).
	URLDSN string

	// DriverDSN is the form modernc.org/sqlite consumes (URL
	// prefix stripped; `file:` form passes through).
	DriverDSN string
}

// ParseDSN normalises the supplied DSN into Config.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, fmt.Errorf("sqlite: dsn is empty")
	}
	return Config{URLDSN: dsn, DriverDSN: URLToDriverDSN(dsn)}, nil
}

// Validate enforces invariants on a parsed Config.
func Validate(cfg Config) error {
	if cfg.DriverDSN == "" {
		return fmt.Errorf("sqlite: empty DSN")
	}
	return nil
}
