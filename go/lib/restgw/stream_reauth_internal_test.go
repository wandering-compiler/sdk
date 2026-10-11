package restgw

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamReauthRetryDelay(t *testing.T) {
	interval := 5 * time.Minute
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, interval, interval}
	for i, w := range want {
		if got := streamReauthRetryDelay(i+1, interval); got != w {
			t.Errorf("delay after %d unanswered probe(s) = %v, want %v", i+1, got, w)
		}
	}
	// Never slower than a healthy probe: an interval below the retry floor
	// caps the retry at the interval.
	if got := streamReauthRetryDelay(1, time.Second); got != time.Second {
		t.Errorf("retry delay with a 1s interval = %v, want 1s", got)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// An unanswered probe reaches the operator — and an outage that fails every
// open stream's probe at once reaches them once per window, not once per
// stream.
func TestReportUnansweredReauth_ReportedAndThrottled(t *testing.T) {
	var buf lockedBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	prevLast := reauthLastReport.Swap(0)
	defer reauthLastReport.Store(prevLast)

	const marker = "stream re-auth: auth backend gave no verdict"
	for i := 0; i < 100; i++ {
		reportUnansweredReauth(context.Background(), errors.New("connection refused"))
	}
	if n := buf.count(marker); n != 1 {
		t.Fatalf("100 unanswered probes in one window produced %d reports, want 1", n)
	}

	reauthLastReport.Store(time.Now().Add(-2 * reauthReportEvery).UnixNano())
	reportUnansweredReauth(context.Background(), errors.New("connection refused"))
	if n := buf.count(marker); n != 2 {
		t.Fatalf("a probe after the window produced no new report (%d total)", n)
	}
}
