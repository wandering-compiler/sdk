package secret_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/secret"
)

const plaintext = "sk_live_super_sensitive_123"

// Every fmt verb must redact — Format covers the lot.
func TestFmtVerbsRedact(t *testing.T) {
	s := secret.New(plaintext)
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v", "%d", "%x"} {
		got := fmt.Sprintf(verb, s)
		if strings.Contains(got, plaintext) {
			t.Errorf("verb %s leaked plaintext: %q", verb, got)
		}
		if got != secret.Redacted {
			t.Errorf("verb %s = %q, want %q", verb, got, secret.Redacted)
		}
	}
}

// A secret nested in a struct must redact under %+v (the classic
// "logged the whole config" leak).
func TestStructRedacts(t *testing.T) {
	cfg := struct {
		Name   string
		APIKey secret.String
	}{Name: "svc", APIKey: secret.New(plaintext)}
	got := fmt.Sprintf("%+v", cfg)
	if strings.Contains(got, plaintext) {
		t.Fatalf("struct print leaked plaintext: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("struct print missing redaction: %q", got)
	}
}

func TestStringerAndGoStringer(t *testing.T) {
	s := secret.New(plaintext)
	if s.String() != secret.Redacted {
		t.Errorf("String() = %q", s.String())
	}
	if s.GoString() != secret.Redacted {
		t.Errorf("GoString() = %q", s.GoString())
	}
}

func TestJSONMarshalRedacts(t *testing.T) {
	b, err := json.Marshal(secret.New(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"`+secret.Redacted+`"` {
		t.Fatalf("MarshalJSON = %s", b)
	}
	if strings.Contains(string(b), plaintext) {
		t.Fatalf("JSON leaked plaintext: %s", b)
	}
}

func TestTextMarshalRedacts(t *testing.T) {
	s := secret.New(plaintext)
	b, err := s.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != secret.Redacted {
		t.Fatalf("MarshalText = %s", b)
	}
}

func TestSlogRedacts(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	logger.Info("boot", "key", secret.New(plaintext))
	if strings.Contains(sb.String(), plaintext) {
		t.Fatalf("slog leaked plaintext: %q", sb.String())
	}
	if !strings.Contains(sb.String(), secret.Redacted) {
		t.Fatalf("slog missing redaction: %q", sb.String())
	}
}

func TestReveal(t *testing.T) {
	s := secret.New(plaintext)
	if s.Reveal() != plaintext {
		t.Fatalf("Reveal() = %q, want %q", s.Reveal(), plaintext)
	}
}

func TestZeroValue(t *testing.T) {
	var s secret.String
	if !s.IsZero() {
		t.Error("zero Secret should be IsZero")
	}
	if s.Reveal() != "" {
		t.Errorf("zero Reveal() = %q", s.Reveal())
	}
	if s.String() != secret.Redacted {
		t.Errorf("zero String() = %q", s.String())
	}
}

// Round-trip: a secret loaded from config (Unmarshal) carries its
// value (Reveal) but still redacts when printed/marshalled.
func TestUnmarshalLoadsButStaysRedacted(t *testing.T) {
	var s secret.String
	if err := s.UnmarshalJSON([]byte(`"` + plaintext + `"`)); err != nil {
		t.Fatal(err)
	}
	if s.Reveal() != plaintext {
		t.Fatalf("after UnmarshalJSON Reveal() = %q", s.Reveal())
	}
	if got := fmt.Sprintf("%v", s); got != secret.Redacted {
		t.Fatalf("loaded secret still printed: %q", got)
	}

	var s2 secret.String
	if err := s2.UnmarshalText([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if s2.Reveal() != plaintext {
		t.Fatalf("after UnmarshalText Reveal() = %q", s2.Reveal())
	}
}

func TestTypedStringAlias(t *testing.T) {
	type Token string
	s := secret.New(Token("abc"))
	if s.Reveal() != Token("abc") {
		t.Fatalf("typed Reveal() = %q", s.Reveal())
	}
	if s.String() != secret.Redacted {
		t.Fatalf("typed String() = %q", s.String())
	}
}
