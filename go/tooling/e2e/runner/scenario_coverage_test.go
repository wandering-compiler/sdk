package runner

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

func TestSetFormat(t *testing.T) {
	prev := format
	t.Cleanup(func() { format = prev })

	SetFormat("json")
	if format != "json" {
		t.Errorf("format = %q, want json", format)
	}
	SetFormat("text")
	if format != "text" {
		t.Errorf("format = %q, want text", format)
	}
	SetFormat("garbage") // unknown falls back to text
	if format != "text" {
		t.Errorf("unknown format = %q, want text fallback", format)
	}
}

func TestExpandHeaders(t *testing.T) {
	scope := runtime.NewRun().NewScope()
	scope.Capture("dev", "device-9")

	// Empty map → nil, no error.
	if out, err := expandHeaders(nil, scope); out != nil || err != nil {
		t.Errorf("empty = %v, %v; want nil, nil", out, err)
	}

	// Literal + interpolated values.
	out, err := expandHeaders(map[string]string{
		"X-Static": "lit",
		"X-Device": "${dev}",
	}, scope)
	if err != nil {
		t.Fatalf("expandHeaders: %v", err)
	}
	if out["X-Static"] != "lit" || out["X-Device"] != "device-9" {
		t.Errorf("expanded headers = %v", out)
	}

	// An unresolvable reference surfaces the expand error.
	if _, err := expandHeaders(map[string]string{"X-Bad": "${nope}"}, scope); err == nil {
		t.Fatal("want error for an unresolvable header interpolation")
	}
}
