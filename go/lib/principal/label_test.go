package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// TestLabel_AbsentOrEmptyIsNotPresent pins the fail-closed read: a rule
// written as `refuse unless the label says X` must see "not present" when
// the label is missing, empty, or there is no metadata at all.
func TestLabel_AbsentOrEmptyIsNotPresent(t *testing.T) {
	cases := map[string]context.Context{
		"no metadata": context.Background(),
		"absent": metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("w17-label-other", "bot")),
		"empty": metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(principal.LabelKey("account_kind"), "")),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			if v, ok := principal.Label(ctx, "account_kind"); ok || v != "" {
				t.Fatalf("Label = (%q, %v), want (\"\", false)", v, ok)
			}
		})
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(principal.LabelKey("account_kind"), "bot"))
	if v, ok := principal.Label(ctx, "account_kind"); !ok || v != "bot" {
		t.Fatalf("Label = (%q, %v), want (\"bot\", true)", v, ok)
	}
	if got := principal.LabelKey("account_kind"); got != "w17-label-account_kind" {
		t.Fatalf("LabelKey = %q", got)
	}
}

// TestReaders_IgnoreOutgoingMetadata pins that only INCOMING metadata is
// read: a value a handler put on its outgoing context (for a downstream
// call) is not the caller's identity.
func TestReaders_IgnoreOutgoingMetadata(t *testing.T) {
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		principal.LabelKey("account_kind"), "bot",
		principal.ScopeKey("org_id"), "7",
	))
	if v, ok := principal.Label(ctx, "account_kind"); ok {
		t.Errorf("Label read outgoing metadata: %q", v)
	}
	if v, ok := principal.Scope(ctx, "org_id"); ok {
		t.Errorf("Scope read outgoing metadata: %q", v)
	}
}

// TestReaders_FirstValueWins pins the multi-valued rule: when a key
// carries several values, the first one is the answer.
func TestReaders_FirstValueWins(t *testing.T) {
	md := metadata.MD{}
	md.Append(principal.LabelKey("account_kind"), "bot", "human")
	md.Append(principal.ScopeKey("org_id"), "7", "8")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if v, _ := principal.Label(ctx, "account_kind"); v != "bot" {
		t.Errorf("Label = %q, want the first value %q", v, "bot")
	}
	if v, _ := principal.Scope(ctx, "org_id"); v != "7" {
		t.Errorf("Scope = %q, want the first value %q", v, "7")
	}
}
