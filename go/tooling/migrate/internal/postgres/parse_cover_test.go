package postgres_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// TestParseDSN_Empty pins that an empty DSN is rejected at parse
// time with the dialect-tagged message.
func TestParseDSN_Empty(t *testing.T) {
	_, err := postgres.ParseDSN("")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestParseDSN_Whitespace pins that an all-whitespace DSN is treated
// as empty — pgx would otherwise produce an opaque connect error.
func TestParseDSN_Whitespace(t *testing.T) {
	_, err := postgres.ParseDSN("   \t  ")
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("expected whitespace error, got %v", err)
	}
}

// TestParseDSN_AcceptsURLAndKeywordForms pins the permissive
// contract: both URL and keyword DSN shapes pass through verbatim
// (pgx owns structural validation at Connect time).
func TestParseDSN_AcceptsURLAndKeywordForms(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@h:5432/db",
		"postgresql://h/db",
		"host=localhost user=u dbname=db",
	} {
		cfg, err := postgres.ParseDSN(dsn)
		if err != nil {
			t.Errorf("ParseDSN(%q): %v", dsn, err)
			continue
		}
		if cfg.DSN != dsn {
			t.Errorf("DSN = %q, want verbatim %q", cfg.DSN, dsn)
		}
	}
}

// TestValidate_RejectsEmpty pins that Validate fails on a zero Config
// (the not-empty invariant beyond ParseDSN).
func TestValidate_RejectsEmpty(t *testing.T) {
	if err := postgres.Validate(postgres.Config{}); err == nil {
		t.Error("expected empty-DSN error on zero Config")
	}
}

// TestValidate_AcceptsParsed pins parse+validate compose: any Config
// ParseDSN produced passes Validate.
func TestValidate_AcceptsParsed(t *testing.T) {
	cfg, err := postgres.ParseDSN("postgres://h/db")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if err := postgres.Validate(cfg); err != nil {
		t.Errorf("Validate of parsed config: %v", err)
	}
}
