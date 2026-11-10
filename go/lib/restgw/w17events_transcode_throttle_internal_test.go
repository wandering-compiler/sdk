package restgw

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// A producer writing envelopes the hub cannot transcode fails every
// delivery of its topic the same way. One line and one report per topic per
// minute say what ten thousand would; the rest are counted, and the next
// report carries the count. Another topic is reported on its own.
func TestEventbusEventSource_TranscodeReportIsThrottledPerTopic(t *testing.T) {
	var sink bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(prev) })

	now := time.Unix(1_000_000, 0)
	e := NewEventbusEventSource(nil, nil, nil, nil)
	e.now = func() time.Time { return now }
	bad := errors.New("bogus envelope")
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		e.reportTranscodeFailure(ctx, "orders", "x.y", bad)
	}
	e.reportTranscodeFailure(ctx, "orders", "x.z", bad)
	if n := strings.Count(sink.String(), "could not transcode"); n != 2 {
		t.Fatalf("101 failures on two topics logged %d lines, want 2 (one per topic):\n%s", n, sink.String())
	}

	now = now.Add(transcodeReportEvery + time.Second)
	sink.Reset()
	e.reportTranscodeFailure(ctx, "orders", "x.y", bad)
	got := sink.String()
	if strings.Count(got, "could not transcode") != 1 || !strings.Contains(got, "99 more") {
		t.Fatalf("the next window's line = %q, want one line counting the 99 suppressed", got)
	}
}
