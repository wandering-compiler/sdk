package factory

import (
	"net"
	"testing"
	"time"
)

// silentServer accepts connections and never says anything — a database
// behind a black-holing proxy, or a port that is not a database at all.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// Opening a connection that never answers is bounded: the run's own deadline
// and LockWait cannot reach a dial made on context.Background(), so the
// factory gives every dial a deadline of its own.
func TestFromTargets_ADialThatNeverAnswersIsBounded(t *testing.T) {
	defer func(d time.Duration) { dialTimeout = d }(dialTimeout)
	dialTimeout = 300 * time.Millisecond
	addr := silentServer(t)
	for _, dsn := range []string{
		"postgres://u@" + addr + "/db?sslmode=disable",
		"mysql://u:p@" + addr + "/db",
	} {
		applierFor := FromTargets([]TargetSpec{{Connection: "main", DSN: dsn}})
		done := make(chan error, 1)
		go func() {
			_, err := applierFor("main")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a server that never answered was accepted", dsn)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: opening the connection had not returned 5 s later", dsn)
		}
	}
}
