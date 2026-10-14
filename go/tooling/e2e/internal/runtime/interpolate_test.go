package runtime

import (
	"strings"
	"testing"
)

func TestDeviceJWKOutsideExpandWithIsNamed(t *testing.T) {
	_, err := Expand(map[string]any{"k": "${device_jwk:bot}"}, NewRun().NewScope())
	if err == nil || !strings.Contains(err.Error(), "only in a step's input") {
		t.Errorf("err = %v, want the place it is allowed named", err)
	}
	got, err := ExpandWith("${device_jwk:bot}", NewRun().NewScope(), map[string]func(string) (any, error){
		DeviceJWKPrefix: func(name string) (any, error) { return "jwk-of-" + name, nil },
	})
	if err != nil || got != "jwk-of-bot" {
		t.Errorf("ExpandWith = %v, %v", got, err)
	}
}
