package runtime

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// After-commit work that outlived its Commit reply (the reply does not wait
// past the caller's deadline) still runs to its end at shutdown: the hooks
// that close what it uses (the event bus) run only after it finished.
func TestGRPCComponent_ShutdownWaitsForAfterCommitWork(t *testing.T) {
	noTLS(t)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	var log eventLog
	addr := freeAddr(t)
	stop := runComponent(t, Config{
		Addr: addr,
		RegisterServices: func(s *grpc.Server) error {
			s.RegisterService(holdDesc(func(ctx context.Context) {
				id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
				if err != nil {
					t.Error(err)
					return
				}
				reg.OnCommit(id, func() {
					time.Sleep(400 * time.Millisecond)
					log.add("after-commit work finished")
				})
				if err := reg.Commit(ctx, id); err != nil {
					t.Error(err)
				}
				log.add("committed")
			}), struct{}{})
			return nil
		},
		Shutdowns: []func(context.Context) error{func(context.Context) error {
			log.add("bus closed")
			return nil
		}},
	})
	conn := plainConn(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := conn.Invoke(ctx, "/test.Hold/Hold", &emptypb.Empty{}, &emptypb.Empty{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("the call answered %v; its Commit must not wait past the deadline", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop = %v", err)
	}
	want := []string{"committed", "after-commit work finished", "bus closed"}
	if got := log.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
