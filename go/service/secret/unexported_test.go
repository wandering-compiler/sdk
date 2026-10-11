package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/secret"
)

// fmt cannot call methods through an unexported struct field, so a Secret
// held in one is printed by reflection, past Format and String. The value
// must not be reachable that way either.
type withUnexported struct {
	Name   string
	apiKey secret.String
	nested struct{ inner secret.String }
	ptr    *secret.String
}

func newWithUnexported() withUnexported {
	s := secret.New(plaintext)
	v := withUnexported{Name: "svc", apiKey: secret.New(plaintext), ptr: &s}
	v.nested.inner = secret.New(plaintext)
	return v
}

func TestUnexportedFieldRedactsInEveryVerb(t *testing.T) {
	v := newWithUnexported()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for _, arg := range []any{v, &v} {
			if got := fmt.Sprintf(verb, arg); strings.Contains(got, plaintext) ||
				strings.Contains(strings.ToLower(got), fmt.Sprintf("%x", plaintext)) {
				t.Errorf("verb %s on %T leaked the secret: %s", verb, arg, got)
			}
		}
	}
}

func TestUnexportedFieldRedactsInSlog(t *testing.T) {
	v := newWithUnexported()
	var buf bytes.Buffer
	for name, h := range map[string]slog.Handler{
		"text": slog.NewTextHandler(&buf, nil),
		"json": slog.NewJSONHandler(&buf, nil),
	} {
		buf.Reset()
		l := slog.New(h)
		l.Info("boot", "cfg", v, "ptr", &v)
		l.Info("boot", slog.Any("cfg", v))
		if strings.Contains(buf.String(), plaintext) {
			t.Errorf("slog %s handler leaked the secret: %s", name, buf.String())
		}
	}
}

// Holding the value behind a pointer must not change what a Secret means:
// the zero value is empty, loading keeps the value, copies share it.
func TestPointerRepresentationKeepsSemantics(t *testing.T) {
	var zero secret.String
	if !zero.IsZero() || zero.Reveal() != "" {
		t.Fatalf("zero Secret: IsZero=%v Reveal=%q", zero.IsZero(), zero.Reveal())
	}
	if !secret.New("").IsZero() {
		t.Fatal("New(\"\") must be zero")
	}
	var loaded secret.String
	if err := json.Unmarshal([]byte(`"`+plaintext+`"`), &loaded); err != nil {
		t.Fatal(err)
	}
	cp := loaded
	if err := cp.UnmarshalText([]byte("other")); err != nil {
		t.Fatal(err)
	}
	if loaded.Reveal() != plaintext || cp.Reveal() != "other" {
		t.Fatalf("reloading a copy changed the original: %q / %q", loaded.Reveal(), cp.Reveal())
	}
}
