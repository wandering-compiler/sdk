package dpop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// A surface reached at several white-label hosts accepts a proof for any of
// the configured origins — never one the Host header names, and never an
// origin outside the set (report 01a205be-eef5).
func TestVerify_AlsoOrigins(t *testing.T) {
	c := newClock()
	v := newVerifier(t, nil, func(cfg *dpop.Config) {
		cfg.Now = c.Now
		cfg.AlsoOrigins = []string{"https://pvsport.example", "https://sport.example:8443"}
	})
	k := mustKey(t)
	for htu, want := range map[string]error{
		testOrigin + "/r":               nil,
		"https://pvsport.example/r":     nil,
		"https://sport.example:8443/r":  nil,
		"https://elsewhere.example/r":   dpop.ErrHTUMismatch,
		"https://pvsport.example/other": dpop.ErrHTUMismatch,
		"http://pvsport.example/r":      dpop.ErrHTUMismatch,
	} {
		cl := claims(c, "")
		cl.HTU = htu
		cl.JTI = "jti-" + htu
		_, err := v.Verify(context.Background(), get(sign(t, k, cl)))
		if (want == nil) != (err == nil) || (want != nil && !errors.Is(err, want)) {
			t.Errorf("htu %s: err %v, want %v", htu, err, want)
		}
	}
	if _, err := dpop.NewVerifier(dpop.Config{Origin: testOrigin, AlsoOrigins: []string{"https://a.example/path"}}); err == nil {
		t.Error("an origin with a path was accepted")
	}
}
