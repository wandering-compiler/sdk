package eventbus

import (
	"context"
	"strings"
	"testing"
)

// A panicking event handler is recovered into a delivery error so
// the consumer goroutine survives (a panic there would otherwise
// crash the whole process) and the transport's NAK/retry path kicks
// in instead.
func TestRecoverHandler_PanicBecomesError(t *testing.T) {
	h := recoverHandler(func(context.Context, string, []byte) error {
		panic("handler blew up on a malformed envelope")
	})

	err := h(context.Background(), "user.created", []byte("x"))
	if err == nil {
		t.Fatal("recoverHandler swallowed a panic with no error — delivery would look successful")
	}
	if !strings.Contains(err.Error(), "user.created") ||
		!strings.Contains(err.Error(), "handler blew up") {
		t.Fatalf("error %q should carry the topic + panic value", err)
	}
}

// A normal handler passes its result through unchanged.
func TestRecoverHandler_PassThrough(t *testing.T) {
	sentinel := context.Canceled
	h := recoverHandler(func(context.Context, string, []byte) error { return sentinel })
	if err := h(context.Background(), "t", nil); err != sentinel {
		t.Fatalf("pass-through err = %v, want %v", err, sentinel)
	}
	ok := recoverHandler(func(context.Context, string, []byte) error { return nil })
	if err := ok(context.Background(), "t", nil); err != nil {
		t.Fatalf("pass-through nil err = %v, want nil", err)
	}
}
