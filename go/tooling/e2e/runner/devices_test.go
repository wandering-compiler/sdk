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

func TestTokenCapture(t *testing.T) {
	cases := []struct {
		expect map[string]any
		want   string
	}{
		{map[string]any{"user_id": map[string]any{"capture": "owner"}, "token": map[string]any{"capture": "auth.token"}}, "auth.token"},
		{map[string]any{"token": map[string]any{"capture": "bob.token", "matcher": "not_empty"}}, "bob.token"},
		{map[string]any{"session_token": map[string]any{"capture": "x.token"}}, ""},
		{map[string]any{"token": "literal"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := tokenCapture(c.expect); got != c.want {
			t.Errorf("tokenCapture(%v) = %q, want %q", c.expect, got, c.want)
		}
	}
}

func mintStep(expect map[string]any) Step {
	return Step{Endpoint: Endpoint{Mint: true}, Expect: expect}
}

// A token a mint captured signs as the mint's device wherever it is copied;
// a token nobody minted signs as its slot's device.
func TestDevices_TokenFollowsItsMint(t *testing.T) {
	d := NewDevices()
	sc := newScope()
	mint := mintStep(map[string]any{"token": map[string]any{"capture": "bob.token"}})
	dv := d.stepDevice(mint, "", nil)
	sc.Capture("bob.token", "tok-b")
	d.learn(dv, mint, nil, sc)
	if got := d.stepDevice(Step{TokenFrom: "copy"}, "tok-b", nil); got != dv {
		t.Error("a copied token did not sign with the device that minted it")
	}
	if d.DeviceFor("tok-b") != dv {
		t.Error("DeviceFor did not find the minting device")
	}
	if d.stepDevice(Step{}, "unknown", nil) != d.named(DefaultTokenSlot) {
		t.Error("an unknown token should sign as the session slot's device")
	}
	if d.stepDevice(mintStep(map[string]any{"token": map[string]any{"capture": "bob.token"}}), "", nil) != dv {
		t.Error("signing in again into a slot did not reuse its device")
	}
	// Step.Device wins over the slot: a second device of the same principal
	// signing into the same slot, and the same device each time it is named.
	session := mintStep(map[string]any{"token": map[string]any{"capture": DefaultTokenSlot}})
	sessionDev := d.stepDevice(session, "", nil)
	phone := session
	phone.Device = "phone"
	phoneDev := d.stepDevice(phone, "", nil)
	if phoneDev == sessionDev || d.stepDevice(phone, "", nil) != phoneDev {
		t.Error("Step.Device did not name its own device")
	}
}

// Only a mint's token field is a token, and a finished mint's other
// captures chain nothing: bob's sign-in whose input happens to carry
// alice's user id is bob's own device.
func TestDevices_AFinishedMintChainsNothing(t *testing.T) {
	d := NewDevices()
	sc := newScope()
	alice := mintStep(map[string]any{
		"token":   map[string]any{"capture": "auth.token"},
		"user_id": map[string]any{"capture": "alice.id"},
	})
	dv := d.stepDevice(alice, "", nil)
	sc.Capture("auth.token", "tok-a")
	sc.Capture("alice.id", "u-1")
	d.learn(dv, alice, nil, sc)

	bob := mintStep(map[string]any{"token": map[string]any{"capture": "bob.token"}})
	bobDev := d.stepDevice(bob, "", map[string]any{"invited_by": "u-1"})
	if bobDev == dv {
		t.Error("bob's mint chained on alice's user id")
	}
	if d.stepDevice(Step{TokenFrom: "bob.token"}, "u-1", nil) != bobDev {
		t.Error("a captured user id was taken for a token of its mint's device")
	}
}

// An unfinished mint (no token yet: an MFA challenge) chains the next mint
// that carries what it captured — strings only — and only once.
func TestDevices_AnUnfinishedMintChainsOnce(t *testing.T) {
	d := NewDevices()
	sc := newScope()
	signIn := mintStep(map[string]any{
		"challenge_id": map[string]any{"capture": "mfa.challenge_id"},
		"remember":     map[string]any{"capture": "mfa.remember"},
	})
	dv := d.stepDevice(signIn, "", nil)
	sc.Capture("mfa.challenge_id", "ch-1")
	sc.Capture("mfa.remember", true)
	d.learn(dv, signIn, nil, sc)

	verify := mintStep(map[string]any{"token": map[string]any{"capture": "auth.token"}})
	if d.stepDevice(verify, "", map[string]any{"flag": "true"}) == dv {
		t.Error("a non-string capture chained a later mint")
	}
	in := map[string]any{"challenge": map[string]any{"id": "ch-1"}}
	if d.stepDevice(verify, "", in) != dv {
		t.Fatal("the mint carrying the challenge did not chain on it")
	}
	// A refused attempt spends nothing: the retry chains again.
	if d.stepDevice(verify, "", in) != dv {
		t.Fatal("a second attempt before success did not chain")
	}
	sc.Capture("auth.token", "tok-a")
	d.learn(dv, verify, in, sc)
	other := mintStep(map[string]any{"token": map[string]any{"capture": "other.token"}})
	if d.stepDevice(other, "", in) == dv {
		t.Error("a spent challenge chained a second successful mint")
	}
}

// A mint that binds no token and chains on nothing signs as a device of its
// own: two of them are two devices, neither the session's.
func TestDevices_AnUnboundMintIsItsOwnDevice(t *testing.T) {
	d := NewDevices()
	a := d.stepDevice(mintStep(nil), "", nil)
	b := d.stepDevice(mintStep(nil), "", nil)
	if a == b || a == d.named(DefaultTokenSlot) {
		t.Error("an unbound mint did not get a device of its own")
	}
}
