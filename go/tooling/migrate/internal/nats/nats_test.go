package nats_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := nats.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestNew_BadSchemeRefuses(t *testing.T) {
	_, err := nats.New(context.Background(), "postgres://x")
	if err == nil || !strings.Contains(err.Error(), "expected nats:// scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestDSNToServer_HostPort(t *testing.T) {
	got, err := nats.DSNToServer("nats://nats.local:4222")
	if err != nil {
		t.Fatalf("DSNToServer: %v", err)
	}
	if got != "nats://nats.local:4222" {
		t.Errorf("got %q", got)
	}
}

func TestDSNToServer_DropsUserInfo(t *testing.T) {
	// Auth flags come in Phase G; for now user info on the URL
	// is silently dropped (not in the --server form).
	got, err := nats.DSNToServer("nats://user:pass@nats.local:4222")
	if err != nil {
		t.Fatalf("DSNToServer: %v", err)
	}
	if strings.Contains(got, "user:pass") {
		t.Errorf("server should drop user info, got %q", got)
	}
}

func TestDSNToServer_BadScheme(t *testing.T) {
	_, err := nats.DSNToServer("redis://x")
	if err == nil || !strings.Contains(err.Error(), "expected nats://") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestDSNToServer_NoHost(t *testing.T) {
	_, err := nats.DSNToServer("nats:///")
	if err == nil || !strings.Contains(err.Error(), "missing host") {
		t.Errorf("expected missing-host error, got %v", err)
	}
}

func TestDSNToServer_MalformedURL(t *testing.T) {
	_, err := nats.DSNToServer("://broken")
	if err == nil || !strings.Contains(err.Error(), "parse url") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestClose_NoOp(t *testing.T) {
	a, err := nats.New(context.Background(), "nats://localhost:4222")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// TestParseArgv_KvPut — bare-token nats kv put line.
func TestParseArgv_KvPut(t *testing.T) {
	argv, err := nats.ParseArgv("nats kv put wc-migrations 20260429T120000Z abc123")
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"nats", "kv", "put", "wc-migrations", "20260429T120000Z", "abc123"}
	if !equalSlices(argv, want) {
		t.Errorf("got %v, want %v", argv, want)
	}
}

// TestParseArgv_StreamEdit — emits double-quoted --description
// value (Go's %q form). Tokeniser strips the quotes.
func TestParseArgv_StreamEdit(t *testing.T) {
	argv, err := nats.ParseArgv(`nats stream edit users --description "renamed to people"`)
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"nats", "stream", "edit", "users", "--description", "renamed to people"}
	if !equalSlices(argv, want) {
		t.Errorf("got %v, want %v", argv, want)
	}
}

// TestParseArgv_UnbalancedQuoteRefuses — emit doesn't generate
// escape sequences, so an unbalanced quote is invalid input.
func TestParseArgv_UnbalancedQuoteRefuses(t *testing.T) {
	_, err := nats.ParseArgv(`nats stream edit users --description "unclosed`)
	if err == nil {
		t.Errorf("expected unbalanced-quote error, got nil")
	}
}

// TestParseArgv_EmptyAndWhitespace — empty / whitespace-only
// inputs yield nil argv. Caller treats both as no-op.
func TestParseArgv_EmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\t"} {
		argv, err := nats.ParseArgv(in)
		if err != nil {
			t.Errorf("ParseArgv(%q): unexpected error %v", in, err)
		}
		if argv != nil {
			t.Errorf("ParseArgv(%q) = %v, want nil", in, argv)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
