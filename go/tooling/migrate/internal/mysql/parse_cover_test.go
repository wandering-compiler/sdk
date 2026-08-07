package mysql_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// TestParseDSN_Empty pins that an empty DSN is rejected at parse time.
func TestParseDSN_Empty(t *testing.T) {
	_, err := mysql.ParseDSN("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestParseDSN_BadScheme pins that ParseDSN surfaces the conversion
// error (wrong scheme) wrapped with the dialect tag rather than
// returning a half-built Config.
func TestParseDSN_BadScheme(t *testing.T) {
	_, err := mysql.ParseDSN("postgres://h/db")
	if err == nil || !strings.Contains(err.Error(), "mysql:") {
		t.Errorf("expected mysql-tagged conversion error, got %v", err)
	}
}

// TestParseDSN_ConvertsAndForcesMultiStatements pins that a valid
// URL DSN yields both forms and the driver form carries the forced
// multiStatements=true flag.
func TestParseDSN_ConvertsAndForcesMultiStatements(t *testing.T) {
	cfg, err := mysql.ParseDSN("mysql://u:p@localhost:3306/db")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if cfg.URLDSN != "mysql://u:p@localhost:3306/db" {
		t.Errorf("URLDSN = %q, want verbatim input", cfg.URLDSN)
	}
	if !strings.HasPrefix(cfg.DriverDSN, "u:p@tcp(localhost:3306)/db?") {
		t.Errorf("DriverDSN shape wrong: %q", cfg.DriverDSN)
	}
	if !strings.Contains(cfg.DriverDSN, "multiStatements=true") {
		t.Errorf("DriverDSN missing multiStatements=true: %q", cfg.DriverDSN)
	}
}

// TestValidate_RejectsEmpty pins that Validate fails on a zero Config
// (either form blank).
func TestValidate_RejectsEmpty(t *testing.T) {
	if err := mysql.Validate(mysql.Config{}); err == nil {
		t.Error("expected empty-DSN error on zero Config")
	}
}

// TestValidate_AcceptsParsed pins parse+validate compose cleanly.
func TestValidate_AcceptsParsed(t *testing.T) {
	cfg, err := mysql.ParseDSN("mysql://localhost/db")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if err := mysql.Validate(cfg); err != nil {
		t.Errorf("Validate of parsed config: %v", err)
	}
}
