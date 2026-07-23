package principal_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
)

// envelopeCtx builds an incoming context carrying the gateway's
// x-w17-user envelope for msg, plus any extra metadata pairs.
// authpb.SignInResp stands in for a project's generated AuthResp —
// the helper is structural (any message with GetUserId), so the
// concrete type only has to satisfy the interface.
func envelopeCtx(t *testing.T, msg proto.Message, kv ...string) context.Context {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pairs := append([]string{"x-w17-user", base64.StdEncoding.EncodeToString(raw)}, kv...)
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// TestUserID_FromScope pins the fast path: when the project declares
// a user_id data scope the gateway threads x-w17-scope-user_id, and
// the helper reads it without touching the envelope.
func TestUserID_FromScope(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-scope-user_id", "u-42"))
	got, err := principal.UserID(ctx, &authpb.SignInResp{})
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if got != "u-42" {
		t.Errorf("UserID = %q, want %q", got, "u-42")
	}
}

// TestUserID_FromEnvelope pins the fallback that makes this usable
// from a project scoped by something OTHER than user_id (e.g. org_id):
// the gateway only threads x-w17-scope-user_id when a user_id scope is
// declared, so the always-present envelope must answer. This is the
// exact case that had no supported API.
func TestUserID_FromEnvelope(t *testing.T) {
	ctx := envelopeCtx(t, &authpb.SignInResp{UserId: "u-7"})
	got, err := principal.UserID(ctx, &authpb.SignInResp{})
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if got != "u-7" {
		t.Errorf("UserID = %q, want %q", got, "u-7")
	}
}

// TestUserID_ScopeWinsOverEnvelope pins precedence: the cheap scope
// read short-circuits the decode.
func TestUserID_ScopeWinsOverEnvelope(t *testing.T) {
	ctx := envelopeCtx(t, &authpb.SignInResp{UserId: "envelope"},
		"x-w17-scope-user_id", "scope")
	got, err := principal.UserID(ctx, &authpb.SignInResp{})
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if got != "scope" {
		t.Errorf("UserID = %q, want scope to win", got)
	}
}

// TestUserID_FailsClosed pins the security-relevant contract: every
// miss is an error, never a silent "". A handler that ignores the
// error must not get a usable-looking zero value.
func TestUserID_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"no metadata at all", context.Background()},
		{"empty metadata", metadata.NewIncomingContext(context.Background(), metadata.MD{})},
		{"empty scope value", metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-w17-scope-user_id", ""))},
		{"envelope not base64", metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-w17-user", "!!!not-base64!!!"))},
		{"envelope not a proto", metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-w17-user", base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff})))},
		{"envelope with empty user_id", envelopeCtxNoT(&authpb.SignInResp{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := principal.UserID(tc.ctx, &authpb.SignInResp{})
			if !errors.Is(err, principal.ErrNoPrincipal) {
				t.Errorf("err = %v, want ErrNoPrincipal", err)
			}
			if got != "" {
				t.Errorf("UserID = %q, want empty on failure", got)
			}
		})
	}
}

func envelopeCtxNoT(msg proto.Message) context.Context {
	raw, _ := proto.Marshal(msg)
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-user", base64.StdEncoding.EncodeToString(raw)))
}

// TestEnvelope_FullClaims pins that a handler needing more than the
// user id (permissions, session token, custom claims) gets the whole
// decoded AuthResp — so no project ever has to hand-roll base64 +
// Unmarshal again.
func TestEnvelope_FullClaims(t *testing.T) {
	ctx := envelopeCtx(t, &authpb.SignInResp{UserId: "u-9"})
	var got authpb.SignInResp
	if err := principal.Envelope(ctx, &got); err != nil {
		t.Fatalf("Envelope: %v", err)
	}
	if got.GetUserId() != "u-9" {
		t.Errorf("UserId = %q, want u-9", got.GetUserId())
	}
}

// TestEnvelope_FailsClosed — absent envelope is ErrNoPrincipal, and
// dst is left untouched.
func TestEnvelope_FailsClosed(t *testing.T) {
	var got authpb.SignInResp
	err := principal.Envelope(context.Background(), &got)
	if !errors.Is(err, principal.ErrNoPrincipal) {
		t.Errorf("err = %v, want ErrNoPrincipal", err)
	}
}

// TestScope covers the generic scope read a project scoped by org_id
// (or tenant_id) uses — the same key convention the generated storage
// layer reads.
func TestScope(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-w17-scope-org_id", "org-1"))
	if got, ok := principal.Scope(ctx, "org_id"); !ok || got != "org-1" {
		t.Errorf("Scope(org_id) = %q,%v want org-1,true", got, ok)
	}
	if got, ok := principal.Scope(ctx, "tenant_id"); ok || got != "" {
		t.Errorf("Scope(tenant_id) = %q,%v want \"\",false", got, ok)
	}
	if _, ok := principal.Scope(context.Background(), "org_id"); ok {
		t.Errorf("Scope with no metadata should be false")
	}
}

// TestScopeKey pins the wire contract the gateway stamps and the
// generated storage layer reads.
func TestScopeKey(t *testing.T) {
	if got := principal.ScopeKey("org_id"); got != "x-w17-scope-org_id" {
		t.Errorf("ScopeKey = %q", got)
	}
}
