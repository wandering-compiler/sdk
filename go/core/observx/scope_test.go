package observx

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// captureSentry initialises observx with Sentry on and returns the events
// it would send.
func captureSentry(t *testing.T) func() []*sentry.Event {
	t.Helper()
	resetForTest(t)
	var mu sync.Mutex
	var events []*sentry.Event
	sentryBeforeSend = func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { sentryBeforeSend = nil })
	if err := MustInit(Config{ServiceName: "svc", SentryDSN: fakeDSN}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = Shutdown(ctx)
	})
	return func() []*sentry.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]*sentry.Event(nil), events...)
	}
}

func silenceLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

func breadcrumbText(e *sentry.Event) string {
	var b strings.Builder
	for _, c := range e.Breadcrumbs {
		b.WriteString(c.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// A refusal recorded for one request must not appear on an exception event
// raised by another, unrelated request.
func TestReportError_DoesNotCarryAnotherRequestsBreadcrumbs(t *testing.T) {
	events := captureSentry(t)
	silenceLog(t)
	t.Setenv("W17_OBSERVX_DEBUG", "true")

	ReportRefusal(context.Background(), errors.New("missing permission other.Caller.Secret"))
	ReportEvent(context.Background(), errors.New("handled for another caller"))
	ReportError(context.Background(), errors.New("unrelated fault"))

	got := events()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if txt := breadcrumbText(got[0]); strings.Contains(txt, "other.Caller") || strings.Contains(txt, "another caller") {
		t.Errorf("event carries another request's breadcrumbs:\n%s", txt)
	}
}

// With a request scope, each request's event carries its own breadcrumbs
// and none of its neighbour's.
func TestRequestScope_KeepsBreadcrumbsPerRequest(t *testing.T) {
	events := captureSentry(t)
	silenceLog(t)

	a := WithRequestScope(context.Background())
	b := WithRequestScope(context.Background())
	if WithRequestScope(a) != a {
		t.Error("a nested entry point replaced its caller's request scope")
	}
	ReportRefusal(a, errors.New("refusal-of-A"))
	ReportRefusal(b, errors.New("refusal-of-B"))
	ReportError(b, errors.New("fault-in-B"))
	ReportError(a, errors.New("fault-in-A"))

	got := events()
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	for i, want := range []struct{ own, other string }{{"refusal-of-B", "refusal-of-A"}, {"refusal-of-A", "refusal-of-B"}} {
		txt := breadcrumbText(got[i])
		if !strings.Contains(txt, want.own) || strings.Contains(txt, want.other) {
			t.Errorf("event %d breadcrumbs = %q, want %q and not %q", i, txt, want.own, want.other)
		}
	}
}

// A refusal adds a span event and a log line; it never marks the span
// failed and never raises an issue. A nil refusal or event does nothing.
func TestReportRefusal_RecordsWithoutRaising(t *testing.T) {
	events := captureSentry(t)
	buf := silenceLog(t)

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("svc").Start(WithRequestScope(context.Background()), "op")
	ReportRefusal(ctx, errors.New("denied"))
	ReportRefusal(ctx, nil)
	ReportEvent(ctx, nil)
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 span, got %d", len(ended))
	}
	if ended[0].Status().Code == codes.Error {
		t.Error("a refusal marked the span failed")
	}
	n := 0
	for _, e := range ended[0].Events() {
		if e.Name == "observx.refusal" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want 1 refusal span event, got %d", n)
	}
	if len(events()) != 0 {
		t.Error("a refusal raised a Sentry event")
	}
	if c := strings.Count(buf.String(), "refused:"); c != 1 {
		t.Errorf("want 1 refusal log line, got %d: %q", c, buf.String())
	}
}

// A flood of refusals produces a bounded number of log lines, and the
// suppressed ones are counted in the next line that is written.
func TestReportRefusal_LogVolumeIsBounded(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background()) })
	buf := silenceLog(t)

	now := time.Unix(1_700_000_000, 0)
	refusalLog.mu.Lock()
	refusalLog.now = func() time.Time { return now }
	refusalLog.mu.Unlock()

	for i := 0; i < 10_000; i++ {
		ReportRefusal(context.Background(), errors.New("bad token"))
	}
	if c := strings.Count(buf.String(), "refused: bad token"); c > logBurst {
		t.Fatalf("10000 refusals in one instant wrote %d lines, want at most %d", c, logBurst)
	}

	now = now.Add(time.Second)
	buf.Reset()
	ReportRefusal(context.Background(), errors.New("bad token"))
	if !strings.Contains(buf.String(), "9950") || !strings.Contains(buf.String(), "suppressed") {
		t.Errorf("the next line does not report the suppressed count: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "refused: bad token") {
		t.Errorf("the refusal itself was not logged once the budget refilled: %q", buf.String())
	}
}

// Suppressed lines still pending at shutdown are counted then.
func TestShutdown_ReportsPendingSuppressedLines(t *testing.T) {
	resetForTest(t)
	if err := MustInit(Config{ServiceName: "svc"}); err != nil {
		t.Fatal(err)
	}
	buf := silenceLog(t)
	for i := 0; i < logBurst+7; i++ {
		ReportError(context.Background(), errors.New("fault"))
	}
	buf.Reset()
	_ = Shutdown(context.Background())
	if !strings.Contains(buf.String(), "7 ") || !strings.Contains(buf.String(), "suppressed") {
		t.Errorf("shutdown did not report the pending count: %q", buf.String())
	}
}
