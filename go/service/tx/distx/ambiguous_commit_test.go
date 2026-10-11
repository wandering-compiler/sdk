package distx_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// A commit that failed in transport is settled, not guessed at. When the
// transaction is still open the commit never took effect: it is rolled back
// and the caller gets an ordinary failure.
func TestCommit_ATransportFailureBeforeTheCommitIsSettledByRollingBack(t *testing.T) {
	c := &scriptedTx{commitErr: status.Error(codes.Unavailable, "connection reset")}
	var emitted []string
	err := distx.Run(context.Background(), c, mainReq(), func(ctx context.Context) error {
		txregistry.DeferUntilCommit(ctx, nil, func() { emitted = append(emitted, "x") })
		return nil
	})
	if err == nil || errors.Is(err, distx.ErrCommitOutcomeUnknown) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want a settled failure saying the transaction was rolled back", err)
	}
	if c.seq() != "begin,commit,rollback" {
		t.Errorf("calls = %s, want begin,commit,rollback", c.seq())
	}
	if len(emitted) != 0 {
		t.Errorf("announced a write that was rolled back: %v", emitted)
	}
}

// When the transaction is gone by the time the rollback arrives, the commit
// may have landed. The caller is told the outcome is unknown — not that the
// write failed — and the after-commit work it is not running is reported,
// not dropped silently.
func TestCommit_AnUnsettledTransportFailureIsReportedAsUnknown(t *testing.T) {
	for _, commitErr := range []error{
		status.Error(codes.Unavailable, "connection reset"),
		status.Error(codes.DeadlineExceeded, "deadline exceeded"),
	} {
		logs := captureLog(t)
		c := &scriptedTx{commitErr: commitErr, rollbackErr: status.Error(codes.NotFound, "unknown tx")}
		var emitted []string
		err := distx.Run(context.Background(), c, mainReq(), func(ctx context.Context) error {
			txregistry.DeferUntilCommit(ctx, nil, func() { emitted = append(emitted, "x") })
			return nil
		})
		if !errors.Is(err, distx.ErrCommitOutcomeUnknown) {
			t.Fatalf("%v: err = %v, want ErrCommitOutcomeUnknown", commitErr, err)
		}
		if status.Code(err) != status.Code(commitErr) {
			t.Errorf("the transport status is lost: %s", status.Code(err))
		}
		if len(emitted) != 0 {
			t.Errorf("announced a write whose commit is unknown: %v", emitted)
		}
		out := logs.String()
		if !strings.Contains(out, "tx-1") || !strings.Contains(out, "unknown") || !strings.Contains(out, "1 piece(s)") {
			t.Errorf("the dropped work was not reported with the transaction: %q", out)
		}
	}
}

// A coordinator's answer is not ambiguous: no settling rollback is sent.
func TestCommit_ARefusalIsNotSettled(t *testing.T) {
	for _, commitErr := range []error{
		status.Error(codes.Aborted, "doomed"),
		status.Error(codes.NotFound, "unknown"),
		errors.New("commit refused"),
	} {
		c := &scriptedTx{commitErr: commitErr}
		err := distx.Run(context.Background(), c, mainReq(), func(context.Context) error { return nil })
		if err == nil || errors.Is(err, distx.ErrCommitOutcomeUnknown) {
			t.Errorf("%v: err = %v", commitErr, err)
		}
		if c.seq() != "begin,commit" {
			t.Errorf("%v: calls = %s, want begin,commit", commitErr, c.seq())
		}
	}
}
