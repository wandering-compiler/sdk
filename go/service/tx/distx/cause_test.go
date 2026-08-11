package distx_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
)

// The end of the chain the consumer actually reads: a Commit on a
// transaction that the auto-rollback interceptor discarded comes back
// NotFound — unchanged, callers branch on the code — but the message
// now names the call that discarded it instead of reporting only that
// the id is unknown.
func TestServer_Commit_AfterAutoRollback_NamesTheCulprit(t *testing.T) {
	client, reg := newClientServer(t)
	ctx := context.Background()

	begun, err := client.Begin(ctx, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// What the interceptor does when a call inside the transaction
	// returns an error the business handler went on to tolerate.
	cause := "a call to /docs.DocumentMutation/UnmarkPaid inside it failed with NotFound"
	if err := reg.RollbackCaused(ctx, begun.GetTxId(), cause); err != nil {
		t.Fatalf("RollbackCaused: %v", err)
	}

	_, err = client.Commit(ctx, &distxpb.CommitRequest{TxId: begun.GetTxId()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Commit = %v (code %s), want NotFound", err, status.Code(err))
	}
	msg := status.Convert(err).Message()
	for _, want := range []string{begun.GetTxId(), "UnmarkPaid", "rolled back"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Commit message = %q, want it to contain %q", msg, want)
		}
	}
}
