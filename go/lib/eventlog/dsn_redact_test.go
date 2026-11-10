package eventlog

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A NATS DSN that does not parse must not come back in the error with its
// password: the url.Error quotes the whole input.
func TestOpenAndServe_ABadDSNIsNotEchoedWithItsPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Open(ctx, "nats://user:p%zzsecret@localhost:1/", RecorderConfig{Domain: "tasks"})
	if err == nil {
		t.Fatal("no error")
	}
	t.Logf("Open: %v", err)
	if strings.Contains(err.Error(), "zzsecret") {
		t.Fatalf("the password is in the error: %v", err)
	}
	var cs Consumers
	err = cs.Serve(ctx, "nats://user:p%zzsecret@localhost:1/", ConsumerConfig{})
	t.Logf("Serve: %v", err)
	if err != nil && strings.Contains(err.Error(), "zzsecret") {
		t.Fatalf("the password is in the Serve error: %v", err)
	}
}
