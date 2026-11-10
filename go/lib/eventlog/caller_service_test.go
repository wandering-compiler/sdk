package eventlog

import (
	"context"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The calling service a private surface admitted is the record's
// caller_service; once the record is finished nothing writes to it.
func TestRecord_SetCallerServiceIsRecorded(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	rec := s.Start(testMethod)
	rec.SetCallerService("spiffe://w17/billing/billing-business")
	rec.Finish(context.Background(), 200)
	rec.SetCallerService("spiffe://w17/other/late")
	var nilRec *Record
	nilRec.SetCallerService("x")
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	if got := decodeEnvelope(t, recs[0]).meta.GetCallerService(); got != "spiffe://w17/billing/billing-business" {
		t.Fatalf("caller_service = %q", got)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// The connection's own log lines (lost, reconnected) name the server without
// the DSN's credential.
func TestOpen_ReconnectLogMasksTheCredential(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	run := func() func() {
		opts := natsserver.DefaultTestOptions
		opts.Port = port
		opts.Username, opts.Password = "svc", "s3cret-pw"
		srv := natsserver.RunServer(&opts)
		return srv.Shutdown
	}
	stop := run()
	buf := &syncBuffer{}
	captureLogTo(t, buf)
	r, err := Open(context.Background(), "nats://svc:s3cret-pw@127.0.0.1:"+strconv.Itoa(port), RecorderConfig{
		Domain:   "tasks",
		Surfaces: map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}},
		Fail:     func(Failure) {},
		Sink: func(w17pb.RequestApi) func(context.Context, *Batch) error {
			return func(context.Context, *Batch) error { return nil }
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	stop()
	stop = run()
	t.Cleanup(stop)
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(buf.String(), "reconnected to") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "reconnected to") {
		t.Fatalf("no reconnect line: %q", out)
	}
	if strings.Contains(out, "s3cret-pw") {
		t.Fatalf("the reconnect log carries the password: %q", out)
	}
}

func captureLogTo(t *testing.T, w *syncBuffer) {
	t.Helper()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// Nothing writes to a finished record (the batcher may be reading it): every
// setter after Finish is a no-op, so a late call cannot change what is
// stored.
func TestRecord_NoSetterWritesAFinishedRecord(t *testing.T) {
	r, c := newTestRecorder(t, map[w17pb.RequestApi]Purposes{w17pb.RequestApi_REQUEST_API_REST: {Audit: true}})
	s := r.Surface(w17pb.RequestApi_REQUEST_API_REST)
	rec := s.Start(testMethod)
	rec.SetRequest(sampleReq())
	rec.SetCaller(authMD("u-1", false, "org-1"))
	rec.Finish(context.Background(), 200)

	rec.SetRequest(nil)
	rec.SetResult(sampleResp(), nil)
	rec.SetCaller(authMD("u-late", false, "org-late"))
	rec.SetCallerOutgoing(context.Background())
	rec.SetCallerIncoming(context.Background())
	rec.FinishStream(context.Background(), context.Canceled)
	rec.FinishUnary(context.Background(), nil, context.Canceled)
	closeRecorder(t, r)
	recs := c.records(w17pb.RequestApi_REQUEST_API_REST)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	m := decodeEnvelope(t, recs[0]).meta
	if m.GetUserId() != "u-1" || m.GetGrpcCode() != 0 || m.GetResponseField() != 0 || m.GetRequestField() == 0 {
		t.Fatalf("a setter after Finish changed the record: user %q code %d request %d response %d",
			m.GetUserId(), m.GetGrpcCode(), m.GetRequestField(), m.GetResponseField())
	}
}
