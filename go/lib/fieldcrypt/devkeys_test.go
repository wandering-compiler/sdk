package fieldcrypt

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// The development keyring is what it says: sha256 of the published sentence,
// a valid version 1 — public, and reproducible by anyone.
func TestDevKeys_IsThePublicSentenceHash(t *testing.T) {
	sum := sha256.Sum256([]byte("w17 public development field key - never a secret"))
	if DevKeys != "1:"+base64.StdEncoding.EncodeToString(sum[:]) {
		t.Errorf("DevKeys = %s", DevKeys)
	}
	if _, err := Parse(DevKeys); err != nil {
		t.Fatal(err)
	}
}

func TestNewKeySpec(t *testing.T) {
	first, err := NewKeySpec("")
	if err != nil || !strings.HasPrefix(first, "1:") {
		t.Fatalf("first = %q, %v", first, err)
	}
	kr, err := Parse(first)
	if err != nil || kr.current != 1 {
		t.Fatalf("parse first: %v", err)
	}
	next, err := NewKeySpec(first)
	if err != nil || !strings.HasPrefix(next, first+",2:") {
		t.Fatalf("next = %q, %v", next, err)
	}
	kr, err = Parse(next)
	if err != nil || kr.current != 2 || len(kr.keys) != 2 {
		t.Fatalf("rotated keyring: %v", err)
	}
	other, _ := NewKeySpec("")
	if other == first {
		t.Error("two fresh keys are equal")
	}
	if _, err := NewKeySpec("not a spec"); err == nil {
		t.Error("a malformed spec was extended")
	}
}
