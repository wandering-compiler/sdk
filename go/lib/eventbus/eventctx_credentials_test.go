package eventbus_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// An event envelope is persisted (broker stream, dead-letter stream) and
// replayed into every subscriber's context, so it must not carry the
// originating request's credentials. What subscribers do need — the verified
// principal, scopes, labels, locale, trace context, declared custom headers —
// must still travel.
func TestMetadataFromContext_DropsCredentials(t *testing.T) {
	credentials := []string{
		"authorization",
		"proxy-authorization",
		"cookie",
		"grpcgateway-cookie",
		"grpcgateway-authorization",
		"x-api-key",
		"api-key",
		"x-auth-token",
		"x-csrf-token",
		"x-amz-security-token",
		"x-client-secret",
		"mcp-session-id",
		"x-w17-credential",
	}
	carried := map[string]string{
		"x-w17-user":                "envelope",
		"x-w17-scope-org_id":        "org-1",
		"x-w17-scope-session_token": "scope-value", // gateway-owned: a declared scope name, not a credential
		"w17-label-team_id":         "t-1",
		"w17-language":              "cs",
		"w17-format-locale":         "cs-CZ",
		"w17-org":                   "org-1",
		"traceparent":               "00-abc-def-01",
		"tracestate":                "k=v",
		"baggage":                   "a=b",
		"x-request-id":              "req-1",
		"x-author-id":               "author-1", // contains "auth" as letters only
		"x-org-hint":                "custom",
	}
	var pairs []string
	for _, k := range credentials {
		pairs = append(pairs, k, "secret-value")
	}
	for k, v := range carried {
		pairs = append(pairs, k, v)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"server tier (incoming)", metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))},
		{"gateway tier (outgoing)", metadata.AppendToOutgoingContext(context.Background(), pairs...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := eventbus.MetadataFromContext(tc.ctx)
			for _, k := range credentials {
				if v, ok := md[k]; ok {
					t.Errorf("Event.Metadata carries credential header %q=%q", k, v)
				}
			}
			for k, want := range carried {
				if got := md[k]; got != want {
					t.Errorf("Event.Metadata[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}

	// Generated subscriber dispatch filters the replayed map with the same
	// predicate, which covers envelopes persisted before the emit side
	// stopped copying credentials.
	for _, k := range credentials {
		if !eventbus.ReservedMetadataKey(k) {
			t.Errorf("ReservedMetadataKey(%q) = false; dispatch would replay it into the subscriber", k)
		}
	}
	for k := range carried {
		if eventbus.ReservedMetadataKey(k) {
			t.Errorf("ReservedMetadataKey(%q) = true; dispatch would drop a key subscribers need", k)
		}
	}
	if !eventbus.ReservedMetadataKey("Authorization") {
		t.Error("ReservedMetadataKey must be case-insensitive")
	}
}

// A repeated key must collapse to the value the subscriber-side reader of
// that key would have picked on the emitting request: last for the locale
// keys (and the metadata convention in general), first for the
// gateway-owned namespaces whose readers take the first value.
func TestMetadataFromContext_RepeatedKeysMatchTheirReaders(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{
		"w17-language":       {"en", "cs"},
		"w17-format-locale":  {"en-US", "cs-CZ"},
		"x-w17-scope-org_id": {"org-first", "org-second"},
		"w17-label-team_id":  {"t-first", "t-second"},
	})

	md := eventbus.MetadataFromContext(ctx)

	// Replay the envelope's metadata the way subscriber dispatch does and ask
	// the locale readers again: they must answer as they did for the emitter.
	replayed := metadata.NewIncomingContext(context.Background(), metadata.New(md))
	if got, want := i18n.LocaleFromContext(replayed), i18n.LocaleFromContext(ctx); got != want {
		t.Errorf("subscriber language = %q, emitter language = %q (Event.Metadata[w17-language] = %q)",
			got, want, md["w17-language"])
	}
	if got, want := i18n.FormatLocaleFromContext(replayed), i18n.FormatLocaleFromContext(ctx); got != want {
		t.Errorf("subscriber format locale = %q, emitter format locale = %q", got, want)
	}
	if got := md["x-w17-scope-org_id"]; got != "org-first" {
		t.Errorf("Event.Metadata[x-w17-scope-org_id] = %q, want the first value (principal.Scope reads the first)", got)
	}
	if got, want := md["w17-label-team_id"], eventbus.LabelsFromContext(ctx)["team_id"]; got != want {
		t.Errorf("Event.Metadata label = %q, Event.Labels label = %q; one envelope, two answers", got, want)
	}
}

// The usual secret-bearing header shapes beyond the token/secret/password
// words: webhook signatures, vendor access and signing keys, short password
// spellings, one-time codes and bearer slots. Each is a whole segment, so
// names that only contain the letters keep travelling.
func TestMetadataFromContext_DropsCommonSecretHeaderShapes(t *testing.T) {
	credentials := []string{
		"x-hub-signature",
		"x-hub-signature-256",
		"x-access-key",
		"x-signing-key",
		"x-user-pass",
		"x-user-pwd",
		"x-otp",
		"x-mfa-otp-code",
		"x-bearer",
	}
	carried := []string{
		"x-keyboard-layout", // "key" only as letters
		"x-passenger-id",    // "pass" only as letters
	}
	var pairs []string
	for _, k := range credentials {
		pairs = append(pairs, k, "secret-value")
	}
	for _, k := range carried {
		pairs = append(pairs, k, "v")
	}
	md := eventbus.MetadataFromContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...)))
	for _, k := range credentials {
		if v, ok := md[k]; ok {
			t.Errorf("Event.Metadata carries credential header %q=%q", k, v)
		}
		if !eventbus.ReservedMetadataKey(k) {
			t.Errorf("ReservedMetadataKey(%q) = false; dispatch would replay it into the subscriber", k)
		}
	}
	for _, k := range carried {
		if md[k] != "v" {
			t.Errorf("Event.Metadata[%q] dropped; only whole segments are flagged", k)
		}
	}
}

// An empty value is no language on either side: the envelope keeps the last
// non-empty value, so the subscriber resolves what the emitter did.
func TestMetadataFromContext_AnEmptyLastLocaleValueIsSkipped(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{
		"w17-language":      {"cs", ""},
		"w17-format-locale": {"cs-CZ", " "},
	})
	md := eventbus.MetadataFromContext(ctx)
	replayed := metadata.NewIncomingContext(context.Background(), metadata.New(md))
	if got, want := i18n.LocaleFromContext(replayed), i18n.LocaleFromContext(ctx); got != want || want != "cs" {
		t.Errorf("subscriber language = %q, emitter language = %q, want cs on both", got, want)
	}
	if got, want := i18n.FormatLocaleFromContext(replayed), i18n.FormatLocaleFromContext(ctx); got != want {
		t.Errorf("subscriber format locale = %q, emitter format locale = %q", got, want)
	}
}
