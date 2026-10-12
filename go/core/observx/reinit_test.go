package observx

import (
	"context"
	"testing"
	"time"
)

// A second MustInit shuts the first one's providers down before it
// replaces them, so the first batch-span goroutine and OTLP connection are
// not left running: a span started on the old provider no longer records.
func TestMustInit_ReinitShutsDownThePreviousProviders(t *testing.T) {
	resetForTest(t)
	t.Setenv("W17_INTERNAL_TLS", "")
	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = Shutdown(ctx)
	})
	mu.Lock()
	first := tracerProvider
	mu.Unlock()
	_, span := first.Tracer("probe").Start(context.Background(), "before")
	if !span.IsRecording() {
		t.Fatal("the first provider does not record before re-init")
	}
	span.End()

	if err := MustInit(Config{ServiceName: "svc", OTelEndpoint: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second := tracerProvider
	mu.Unlock()
	if second == first {
		t.Fatal("re-init kept the first provider")
	}
	if _, span := first.Tracer("probe").Start(context.Background(), "after"); span.IsRecording() {
		t.Error("the first provider still records after re-init; it was not shut down")
	}
}
