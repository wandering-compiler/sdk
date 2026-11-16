package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// A business method reached over the wire inside a transaction ANOTHER
// process runs — here the harness begins it, as a Go caller would, and the
// request carries its w17-tx-id — holds its event until that transaction
// commits, and drops it when it rolls back. (CreateNote runs its own
// transaction for the write; the event answers to the caller's.)
func TestARemoteTransactionHoldsTheCalleesEventUntilItsCommit(t *testing.T) {
	cc, err := grpc.NewClient("127.0.0.1:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	coordinator := distxpb.NewW17DistributedTransactionClient(cc)
	for _, commit := range []bool{true, false} {
		text := "remote-rollback"
		if commit {
			text = "remote-commit"
		}
		t.Run(text, func(t *testing.T) {
			handle, _, err := distx.Begin(context.Background(), coordinator, &distxpb.BeginRequest{ConnectionName: "main"})
			if err != nil {
				t.Fatal(err)
			}
			mark := env.eventCount()
			ctx, cancel := timeout(gatewayCtx(t, "w17-tx-id", handle.TxID()), 10*time.Second)
			_, err = env.call(ctx, "CreateNote", set(env.msg("CreateNoteRequest"), map[string]any{"text": text}), "Note")
			cancel()
			if err != nil {
				_ = handle.Rollback(context.Background())
				t.Fatal(err)
			}
			time.Sleep(500 * time.Millisecond)
			if got := noteCreated(t, env.eventsSince(mark)); len(got) != 0 {
				t.Errorf("events %v arrived before the caller's transaction ended", got)
			}
			if commit {
				if err := handle.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if err := handle.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := []string(nil)
			if commit {
				want = []string{text}
			}
			deadline := time.Now().Add(5 * time.Second)
			for commit && len(noteCreated(t, env.eventsSince(mark))) == 0 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			// A little longer, so a duplicate (or a late event of the
			// rollback) has its chance to show.
			time.Sleep(500 * time.Millisecond)
			if got := noteCreated(t, env.eventsSince(mark)); !slices.Equal(got, want) {
				t.Errorf("events after the caller's transaction ended = %v, want %v", got, want)
			}
		})
	}
}
