package grpcerr

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/lib/pq"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func resetOutageReports(t *testing.T) {
	t.Helper()
	outageReports.mu.Lock()
	outageReports.last = time.Time{}
	outageReports.mu.Unlock()
}

func detailCode(err error) string {
	for _, d := range status.Convert(err).Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			return ed.GetCode()
		}
	}
	return ""
}

// A database the call never reached — through each real driver — answers
// UNAVAILABLE with DATABASE_UNAVAILABLE, the code w17 clients retry by
// themselves, and an outage is reported once, not once per call.
func TestWrap_AnUnreachableDatabaseIsUnavailableAndReportedOnce(t *testing.T) {
	resetOutageReports(t)
	buf := captureLog(t)
	addr := closedAddr(t)
	host, port, _ := net.SplitHostPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var failures []error
	for _, open := range []struct{ driver, dsn string }{
		{"postgres", "postgres://u:p@" + addr + "/db?sslmode=disable&connect_timeout=2"},
		{"mysql", "u:p@tcp(" + addr + ")/db?timeout=2s"},
	} {
		db, err := sql.Open(open.driver, open.dsn)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(ctx, "SELECT 1")
		_ = db.Close()
		if err == nil {
			t.Fatalf("%s reached a closed port", open.driver)
		}
		failures = append(failures, err)
	}
	if _, err := pgconn.Connect(ctx, "postgres://u:p@"+host+":"+port+"/db?sslmode=disable&connect_timeout=2"); err != nil {
		failures = append(failures, err)
	}

	for _, e := range failures {
		got := Wrap(ctx, "/svc/Read", e, nil, DialectPostgres)
		if status.Code(got) != codes.Unavailable || detailCode(got) != CodeDatabaseUnavailable {
			t.Errorf("%T %v -> %v (%s), want Unavailable / %s", e, e, status.Code(got), detailCode(got), CodeDatabaseUnavailable)
		}
	}
	if n := strings.Count(buf.String(), "the database is unreachable"); n != 1 {
		t.Errorf("an outage across %d calls was reported %d times, want once; log: %q", len(failures), n, buf.String())
	}
}

// The report comes again once the throttle window has passed, so an outage
// that lasts is not reported only at its start.
func TestWrap_AnOngoingOutageIsReportedAgainAfterTheWindow(t *testing.T) {
	resetOutageReports(t)
	prev := outageReportEvery
	outageReportEvery = 50 * time.Millisecond
	t.Cleanup(func() { outageReportEvery = prev })
	buf := captureLog(t)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	_ = Wrap(context.Background(), "/svc/Read", refused, nil, DialectPostgres)
	_ = Wrap(context.Background(), "/svc/Read", refused, nil, DialectPostgres)
	time.Sleep(60 * time.Millisecond)
	_ = Wrap(context.Background(), "/svc/Read", refused, nil, DialectPostgres)
	if n := strings.Count(buf.String(), "the database is unreachable"); n != 2 {
		t.Errorf("reported %d times over two windows, want 2", n)
	}
}

// A connection that broke during the call leaves its outcome unknown, and
// a server that answered (an authentication failure) was reached: neither
// is UNAVAILABLE, which a client would retry, and both are reported.
func TestWrap_ABrokenConnectionOrAnAnsweringServerIsNotUnavailable(t *testing.T) {
	for _, e := range []error{
		driver.ErrBadConn,
		io.ErrUnexpectedEOF,
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
		fmt.Errorf("failed to connect: %w", &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"}),
	} {
		resetOutageReports(t)
		buf := captureLog(t)
		got := Wrap(context.Background(), "/svc/Write", e, nil, DialectPostgres)
		if status.Code(got) == codes.Unavailable {
			t.Errorf("%T %v -> Unavailable; a call that may have run must not be retried for the caller", e, e)
		}
		if buf.Len() == 0 {
			t.Errorf("%T %v was not reported", e, e)
		}
	}
}
