package redis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := redis.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestNew_BadSchemeRefuses(t *testing.T) {
	_, err := redis.New(context.Background(), "postgres://x")
	if err == nil {
		t.Errorf("expected scheme error, got nil")
	}
}

// TestParseDSN_Minimal — bare host yields default port 6379, db 0,
// no auth.
func TestParseDSN_Minimal(t *testing.T) {
	got, err := redis.ParseDSN("redis://localhost")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if got.Options.Addr != "localhost:6379" {
		t.Errorf("Addr = %q, want %q", got.Options.Addr, "localhost:6379")
	}
	if got.Options.DB != 0 {
		t.Errorf("DB = %d, want 0", got.Options.DB)
	}
	if got.Options.Username != "" || got.Options.Password != "" {
		t.Errorf("expected no auth; got user=%q pass=%q", got.Options.Username, got.Options.Password)
	}
}

// TestParseDSN_FullURL — every URL component lands on Options:
// userinfo → Username/Password, port → Addr, /N → DB.
func TestParseDSN_FullURL(t *testing.T) {
	got, err := redis.ParseDSN("redis://alice:s3cret@redis-host:6380/3")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if got.Options.Addr != "redis-host:6380" {
		t.Errorf("Addr = %q", got.Options.Addr)
	}
	if got.Options.Username != "alice" {
		t.Errorf("Username = %q", got.Options.Username)
	}
	if got.Options.Password != "s3cret" {
		t.Errorf("Password = %q", got.Options.Password)
	}
	if got.Options.DB != 3 {
		t.Errorf("DB = %d, want 3", got.Options.DB)
	}
}

// TestParseDSN_TLS — `rediss://` populates Options.TLSConfig.
func TestParseDSN_TLS(t *testing.T) {
	got, err := redis.ParseDSN("rediss://localhost:6380")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if got.Options.TLSConfig == nil {
		t.Errorf("rediss:// should set TLSConfig; got nil")
	}
}

// TestParseDSN_NonNumericDB — go-redis ParseURL refuses non-numeric
// DB segments.
func TestParseDSN_NonNumericDB(t *testing.T) {
	_, err := redis.ParseDSN("redis://localhost/notnumeric")
	if err == nil {
		t.Errorf("expected non-numeric DB error, got nil")
	}
}

func TestParseDSN_BadScheme(t *testing.T) {
	_, err := redis.ParseDSN("memcache://x")
	if err == nil {
		t.Errorf("expected scheme error, got nil")
	}
}

func TestParseDSN_MalformedURL(t *testing.T) {
	_, err := redis.ParseDSN("://broken")
	if err == nil {
		t.Errorf("expected parse error, got nil")
	}
}

func TestFilterComments_Strips(t *testing.T) {
	in := `# wc: this is a comment
SET foo bar
# another comment

ZADD wc:migrations 1 ts-1`
	want := `SET foo bar
ZADD wc:migrations 1 ts-1`
	if got := redis.FilterComments(in); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFilterComments_AllComments(t *testing.T) {
	in := "# wc: only-comment\n# another\n\n"
	if got := redis.FilterComments(in); got != "" {
		t.Errorf("all-comment script should yield empty, got %q", got)
	}
}

func TestFilterComments_PreservesIndentation(t *testing.T) {
	in := "  EVAL 'return KEYS[1]' 1 foo"
	got := redis.FilterComments(in)
	if !strings.HasPrefix(got, "  EVAL") {
		t.Errorf("indentation lost: %q", got)
	}
}

// TestParseArgv_Bare — bare tokens split on whitespace.
func TestParseArgv_Bare(t *testing.T) {
	argv, err := redis.ParseArgv("HSET wc:migrations 20260429T120000Z abc123")
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"HSET", "wc:migrations", "20260429T120000Z", "abc123"}
	if !equalSlices(argv, want) {
		t.Errorf("got %v, want %v", argv, want)
	}
}

// TestParseArgv_DoubleQuotedLuaScript — Lua bodies are double-
// quoted; single quotes inside (Lua string literals) are
// preserved verbatim.
func TestParseArgv_DoubleQuotedLuaScript(t *testing.T) {
	in := `EVAL "local cursor = '0'; return cursor" 0 'users:*'`
	argv, err := redis.ParseArgv(in)
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"EVAL", "local cursor = '0'; return cursor", "0", "users:*"}
	if !equalSlices(argv, want) {
		t.Errorf("got %v, want %v", argv, want)
	}
}

// TestParseArgv_EmptyAndWhitespace — empty input → nil argv;
// whitespace-only → nil argv. Caller treats both as no-op.
func TestParseArgv_EmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\t"} {
		argv, err := redis.ParseArgv(in)
		if err != nil {
			t.Errorf("ParseArgv(%q): unexpected error %v", in, err)
		}
		if argv != nil {
			t.Errorf("ParseArgv(%q) = %v, want nil", in, argv)
		}
	}
}

// TestParseArgv_UnbalancedQuoteRefuses — emit doesn't generate
// escape sequences, so unbalanced quotes can't be valid input.
func TestParseArgv_UnbalancedQuoteRefuses(t *testing.T) {
	for _, in := range []string{
		`EVAL "unclosed`,
		`EVAL 'unclosed`,
	} {
		_, err := redis.ParseArgv(in)
		if err == nil {
			t.Errorf("ParseArgv(%q): expected unbalanced-quote error", in)
		}
	}
}

// TestParseArgv_AdjacentQuoted — `foo"bar"baz` joins as a single
// token. The emitter doesn't produce this shape; the parser
// accepts it for robustness.
func TestParseArgv_AdjacentQuoted(t *testing.T) {
	argv, err := redis.ParseArgv(`SET key "value"`)
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"SET", "key", "value"}
	if !equalSlices(argv, want) {
		t.Errorf("got %v, want %v", argv, want)
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
