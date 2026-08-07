package nats

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// TestDropFlag pins the bare-flag stripper used to drop
// `--force` from `stream rm` lines.
func TestDropFlag(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		flag string
		want []string
	}{
		{"removes single", []string{"--force", "users"}, "--force", []string{"users"}},
		{"removes all instances", []string{"--force", "a", "--force", "b"}, "--force", []string{"a", "b"}},
		{"absent flag unchanged", []string{"users"}, "--force", []string{"users"}},
		{"empty input", nil, "--force", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dropFlag(tc.in, tc.flag)
			if len(got) != len(tc.want) {
				t.Fatalf("dropFlag = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("dropFlag[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestFlagValue pins the `--flag <value>` scanner used for
// `--description`.
func TestFlagValue(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		flag    string
		wantVal string
		wantOK  bool
	}{
		{"found", []string{"--description", "renamed"}, "--description", "renamed", true},
		{"found mid-list", []string{"users", "--description", "x"}, "--description", "x", true},
		{"absent", []string{"users"}, "--description", "", false},
		{"flag at end has no value", []string{"users", "--description"}, "--description", "", false},
		{"empty argv", nil, "--description", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, ok := flagValue(tc.argv, tc.flag)
			if ok != tc.wantOK || val != tc.wantVal {
				t.Errorf("flagValue = (%q,%v), want (%q,%v)", val, ok, tc.wantVal, tc.wantOK)
			}
		})
	}
}

// TestInvalidateBucket pins that a cached KV handle is evicted so
// the next reference re-opens it.
func TestInvalidateBucket(t *testing.T) {
	a := &Applier{buckets: map[string]jetstream.KeyValue{"b": nil, "c": nil}}
	a.invalidateBucket("b")
	if _, ok := a.buckets["b"]; ok {
		t.Error("bucket b should have been evicted")
	}
	if _, ok := a.buckets["c"]; !ok {
		t.Error("bucket c should remain cached")
	}
	// Evicting a missing key is a no-op (no panic).
	a.invalidateBucket("missing")
}

// TestValidate_NATSConfig pins the empty-Server guard on a parsed
// Config.
func TestValidate_NATSConfig(t *testing.T) {
	if err := Validate(Config{}); err == nil {
		t.Error("empty Server should be refused")
	}
	if err := Validate(Config{Server: "nats://h:4222"}); err != nil {
		t.Errorf("valid Config refused: %v", err)
	}
}

// TestParseDSN_Empty pins the explicit empty-DSN guard.
func TestParseDSN_Empty(t *testing.T) {
	if _, err := ParseDSN(""); err == nil {
		t.Error("empty DSN should be refused")
	}
}

// TestParseDSN_OK pins the happy path: a well-formed DSN yields a
// usable Server.
func TestParseDSN_OK(t *testing.T) {
	cfg, err := ParseDSN("nats://nats.local:4222")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if cfg.Server != "nats://nats.local:4222" || cfg.DSN == "" {
		t.Errorf("unexpected Config %+v", cfg)
	}
}

// TestParseDSN_BadScheme pins that the wrapper surfaces the
// scheme error from DSNToServer.
func TestParseDSN_BadScheme(t *testing.T) {
	if _, err := ParseDSN("redis://x"); err == nil {
		t.Error("expected scheme error")
	}
}

// TestParseArgv_SingleQuoted pins the single-quote branch of the
// tokeniser (accepted for parity with the Redis parser even
// though emit never produces it). Single quotes wrap a run with
// spaces into one token.
func TestParseArgv_SingleQuoted(t *testing.T) {
	argv, err := ParseArgv(`nats kv put b k 'a value'`)
	if err != nil {
		t.Fatalf("ParseArgv: %v", err)
	}
	want := []string{"nats", "kv", "put", "b", "k", "a value"}
	if len(argv) != len(want) {
		t.Fatalf("got %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

// TestParseArgv_UnbalancedSingleQuote pins the single-quote
// error branch.
func TestParseArgv_UnbalancedSingleQuote(t *testing.T) {
	if _, err := ParseArgv(`nats kv put b k 'unclosed`); err == nil {
		t.Error("expected unbalanced single-quote error")
	}
}
