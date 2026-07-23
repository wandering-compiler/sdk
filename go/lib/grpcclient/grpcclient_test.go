package grpcclient_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// Default posture: the stack-wide switch off (unset) dials PLAIN
// h2c — the internal mesh is trusted / infra-secured, so plaintext
// between services is the intended default, not an accident.
func TestTLSDialOption_Default_Plain(t *testing.T) {
	opt, err := grpcclient.TLSDialOption(func(string) string { return "" })
	if err != nil {
		t.Fatalf("default lookup: %v", err)
	}
	if opt == nil {
		t.Fatal("opt is nil; want plain (insecure) DialOption")
	}
}

// Switch on: W17_INTERNAL_TLS=on dials TLS (server-auth against
// system roots when no CA is pinned).
func TestTLSDialOption_SwitchOn_TLS(t *testing.T) {
	env := map[string]string{"W17_INTERNAL_TLS": "on"}
	opt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("W17_INTERNAL_TLS=on: %v", err)
	}
	if opt == nil {
		t.Fatal("opt is nil; want TLS DialOption")
	}
}

// The switch accepts the documented truthy spellings and treats
// everything else (including junk) as off.
func TestInternalTLSEnabled_Spellings(t *testing.T) {
	on := []string{"on", "ON", "true", "True", "1", "yes", " on "}
	for _, v := range on {
		if !grpcclient.InternalTLSEnabled(v) {
			t.Errorf("%q should be ON", v)
		}
	}
	off := []string{"", "off", "false", "0", "no", "tls", "maybe"}
	for _, v := range off {
		if grpcclient.InternalTLSEnabled(v) {
			t.Errorf("%q should be OFF", v)
		}
	}
}

// Switch off wins even when CA / cert knobs are present — nothing is
// read until the stack turns internal TLS on. Guards against a stray
// cert path silently forcing TLS.
func TestTLSDialOption_OffIgnoresKnobs(t *testing.T) {
	env := map[string]string{
		"W17_INTERNAL_TLS_CA":   "/nonexistent/ca.pem",
		"W17_INTERNAL_TLS_CERT": "/some/cert.pem",
	}
	opt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("switch off must ignore knobs, got: %v", err)
	}
	if opt == nil {
		t.Fatal("opt is nil; want plain DialOption")
	}
}

// Missing CA file (switch on) → startup error so misconfiguration
// surfaces immediately, naming the env var.
func TestTLSDialOption_MissingCAFile_Errors(t *testing.T) {
	env := map[string]string{
		"W17_INTERNAL_TLS":    "on",
		"W17_INTERNAL_TLS_CA": "/nonexistent/ca.pem",
	}
	_, err := grpcclient.TLSDialOption(lookup(env))
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
	if !strings.Contains(err.Error(), "W17_INTERNAL_TLS_CA") {
		t.Errorf("error should mention env var name; got %v", err)
	}
}

// Half-configured mTLS (cert without key) → startup error.
func TestTLSDialOption_HalfMTLS_Errors(t *testing.T) {
	env := map[string]string{
		"W17_INTERNAL_TLS":      "on",
		"W17_INTERNAL_TLS_CERT": "/some/cert.pem",
	}
	_, err := grpcclient.TLSDialOption(lookup(env))
	if err == nil {
		t.Fatal("expected error: cert without key")
	}
	if !strings.Contains(err.Error(), "set together") {
		t.Errorf("error should explain both env vars must be set; got %v", err)
	}
}

// Nil lookup short-circuits to plain — the in-process / test shortcut.
func TestTLSDialOption_NilLookup_Plain(t *testing.T) {
	opt, err := grpcclient.TLSDialOption(nil)
	if err != nil {
		t.Fatalf("nil lookup: %v", err)
	}
	if opt == nil {
		t.Fatal("opt is nil")
	}
}

// Dial returns a non-blocking ClientConn even when the address is
// unreachable. First RPC surfaces the error — startup stays fast.
// Verifies the convention-driven opts (TLS + otelgrpc) are wired and
// don't break the dial path.
func TestDial_NonBlocking(t *testing.T) {
	conn, err := grpcclient.Dial("127.0.0.1:1", func(string) string { return "" })
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if conn == nil {
		t.Fatal("conn is nil")
	}
}

// A bad TLS config (switch on + unreadable CA) propagates out of Dial
// at startup rather than at first-RPC.
func TestDial_TLSConfigError_Propagates(t *testing.T) {
	env := map[string]string{
		"W17_INTERNAL_TLS":    "on",
		"W17_INTERNAL_TLS_CA": "/nonexistent/ca.pem",
	}
	_, err := grpcclient.Dial("127.0.0.1:1", lookup(env))
	if err == nil {
		t.Fatal("expected error: TLS config bad")
	}
}

// lookup wraps an env fixture map as a LookupFunc.
func lookup(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
