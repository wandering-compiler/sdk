package sqlite_test

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// SQLite cannot resume a post-transaction half: it does not offer the
// resume capability, and running a non-empty one warns, naming the
// migration; a migration without one says nothing.
func TestApply_PostTxWarnsThatItIsNotResumable(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	ctx := context.Background()
	a, err := sqlite.New(ctx, "file:"+filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if _, ok := any(a).(migrate.ResumableApplier); ok {
		t.Error("the SQLite applier claims it can resume a post-transaction half")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260101T000000Z", UpSql: "CREATE TABLE t (id INTEGER);"}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a migration without a post-transaction half warned: %q", buf.String())
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260201T000000Z", UpSql: "CREATE TABLE u (id INTEGER);", UpPostTx: "CREATE INDEX u_id ON u (id);"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "NOT resumable") || !strings.Contains(out, "20260201T000000Z") {
		t.Errorf("no not-resumable warning naming the migration: %q", out)
	}
}
