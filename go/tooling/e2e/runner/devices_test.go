package runner

import (
	"strings"
	"testing"
)

// A mint step presents no token: token_from on one is refused, and the
// session is not injected even when the endpoint requires auth.
func TestStepToken_MintPresentsNoToken(t *testing.T) {
	sc := newScope()
	sc.Capture(DefaultTokenSlot, "tok-1")
	mint := Step{Endpoint: Endpoint{Ref: "auth.SignIn", AuthRequired: true, Mint: true}}
	if tok, err := stepToken(mint, sc); err != nil || tok != "" {
		t.Errorf("mint step token = %q, %v; want none", tok, err)
	}
	mint.TokenFrom = DefaultTokenSlot
	if _, err := stepToken(mint, sc); err == nil || !strings.Contains(err.Error(), "mint") {
		t.Errorf("token_from on a mint: err = %v, want a refusal", err)
	}
}

func TestMintSlot(t *testing.T) {
	cases := []struct {
		expect map[string]any
		want   string
	}{
		{map[string]any{"user_id": map[string]any{"capture": "owner"}, "token": map[string]any{"capture": "auth.token"}}, "auth.token"},
		{map[string]any{"user_id": map[string]any{"capture": "a_owner"}, "session": map[string]any{"capture": "bob.token"}}, "bob.token"},
		{map[string]any{"user": map[string]any{"id": map[string]any{"capture": "uid"}}}, "uid"},
		{map[string]any{"t": map[string]any{"capture": "outer", "match": map[string]any{"x": map[string]any{"capture": "inner_token"}}}}, "inner_token"},
		{map[string]any{"n": map[string]any{"matcher": "not_empty"}}, DefaultTokenSlot},
		{nil, DefaultTokenSlot},
	}
	for _, c := range cases {
		if got := mintSlot(Step{Expect: c.expect}); got != c.want {
			t.Errorf("mintSlot(%v) = %q, want %q", c.expect, got, c.want)
		}
	}
}

// Without a device, a token's device is the slot it is read from; a token a
// mint captured belongs to the mint's device wherever it is copied to.
func TestDevices_TokenFollowsItsMint(t *testing.T) {
	d := NewDevices()
	sc := newScope()
	mint := Step{Endpoint: Endpoint{Mint: true}, Expect: map[string]any{"token": map[string]any{"capture": "bob.token"}}}
	dv := d.stepDevice(mint, "")
	sc.Capture("bob.token", "tok-b")
	d.learn(dv, mint, sc)
	if got := d.stepDevice(Step{TokenFrom: "copy"}, "tok-b"); got != dv {
		t.Error("a copied token did not sign with the device that minted it")
	}
	if got := d.DeviceFor("tok-b"); got != dv {
		t.Error("DeviceFor did not find the minting device")
	}
	if d.stepDevice(Step{}, "unknown") == dv || d.stepDevice(Step{}, "unknown") != d.named(DefaultTokenSlot) {
		t.Error("an unknown token should sign as the session slot's device")
	}
	if d.stepDevice(Step{Device: "phone", Endpoint: Endpoint{Mint: true}}, "") == d.named(DefaultTokenSlot) {
		t.Error("Step.Device did not name its own device")
	}
}
