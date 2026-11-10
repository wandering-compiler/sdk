package i18n_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

func langCtx(vals ...string) context.Context {
	md := metadata.MD{}
	for _, v := range vals {
		md.Append(i18n.LanguageMetadataKey, v)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func formatCtx(v string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(i18n.FormatLocaleMetadataKey, v))
}

// One bad member does not void a whole Accept-Language list: the members
// that parse are still negotiated, in q order.
func TestNegotiation_ABadMemberDoesNotVoidTheList(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs", "hello", "ahoj")))
	i18n.Register("en", []byte(minimalPO("en", "hello", "hi")))
	for _, v := range []string{"cs,klingon-xx-yy-zz-qq", "fr,cs;q=0.8,en;q=abc", "xx-!!,cs"} {
		if got := i18n.T(langCtx(v), "hello", nil); got != "ahoj" {
			t.Errorf("T(%q) = %q, want ahoj (cs negotiated from the members that parse)", v, got)
		}
	}
	want := i18n.FormatLocaleFromContext(formatCtx("cs-CZ,en;q=0.5"))
	if got := i18n.FormatLocaleFromContext(formatCtx("cs-CZ,en;q=abc")); got != want {
		t.Errorf("FormatLocaleFromContext(cs-CZ,en;q=abc) = %q, want %q (as for the list without the bad member)", got, want)
	}
}

// An empty w17-language value is no language: the last non-empty value
// wins, and with none the default applies.
func TestLocaleFromContext_SkipsEmptyValues(t *testing.T) {
	resetState(t)
	i18n.SetDefaultLanguage("cs")
	if got := i18n.LocaleFromContext(langCtx("")); got != "cs" {
		t.Errorf("LocaleFromContext(\"\") = %q, want the default cs", got)
	}
	if got := i18n.LocaleFromContext(langCtx("de", "  ")); got != "de" {
		t.Errorf("LocaleFromContext(de, blank) = %q, want de", got)
	}
}

// A catalog registered under a deprecated tag (iw) serves a request the
// negotiation canonicalises (iw-IL → he-IL → he), and the reverse.
func TestRegisterCatalog_ServesTheCanonicalTag(t *testing.T) {
	resetState(t)
	i18n.Register("iw", []byte(minimalPO("iw", "hello", "shalom")))
	for _, v := range []string{"iw", "iw-IL", "he", "he-IL,en;q=0.5"} {
		if got := i18n.T(langCtx(v), "hello", nil); got != "shalom" {
			t.Errorf("T(%q) = %q, want shalom", v, got)
		}
	}
}

// A huge language value is not parsed whole on every T: only its first
// 512 bytes are considered, so a request cannot make each translation cost
// a megabyte of parsing.
func TestNegotiation_BoundsTheValueItParses(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs", "hello", "ahoj")))
	huge := "cs," + strings.Repeat("en-US;q=0.1,", 80000)
	ctx := langCtx(huge)
	start := time.Now()
	for range 200 {
		if got := i18n.T(ctx, "hello", nil); got != "ahoj" {
			t.Fatalf("T(huge) = %q, want ahoj", got)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("200 translations of a %d-byte value took %v; the value is parsed whole", len(huge), d)
	}
}

type capturingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// Register logs the problems RegisterCatalog would return, and a second,
// different SetDefaultLanguage is logged; the same value twice is not.
func TestRegisterAndSetDefaultLanguage_LogTheirProblems(t *testing.T) {
	resetState(t)
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	i18n.Register("cs", []byte("msgid \"a\"\nmsgstr \"b\"\nnot po at all\n"))
	i18n.SetDefaultLanguage("cs")
	i18n.SetDefaultLanguage("CS")
	i18n.SetDefaultLanguage("de")

	h.mu.Lock()
	defer h.mu.Unlock()
	joined := strings.Join(h.msgs, "\n")
	if !strings.Contains(joined, "catalog registered with problems") {
		t.Errorf("Register logged no problem for a malformed catalog: %q", joined)
	}
	if n := strings.Count(joined, "default language set twice"); n != 1 {
		t.Errorf("default-language warnings = %d, want 1 (cs → de; CS is the same language): %q", n, joined)
	}
}

// Registration and translation from many goroutines at once are race-free
// (run under -race).
func TestCatalogs_ConcurrentRegisterAndTranslate(t *testing.T) {
	resetState(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				i18n.Register("cs", []byte(minimalPO("cs", "hello", "ahoj")))
				i18n.SetDefaultLanguage("en")
			}
		}()
		go func() {
			defer wg.Done()
			ctx := langCtx([]string{"cs", "en", "cs-CZ,en;q=0.5"}[i%3])
			for range 200 {
				_ = i18n.T(ctx, "hello", map[string]string{"x": "y"})
				_ = i18n.FormatLocaleFromContext(ctx)
				_ = i18n.LocaleFromContext(ctx)
			}
		}()
	}
	wg.Wait()
}
