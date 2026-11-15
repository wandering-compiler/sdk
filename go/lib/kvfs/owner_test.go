package kvfs_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
)

// TestPut_RecordsTheRequestsOwner: a file the business tier stores while
// serving a caller carries that caller's data scopes, like an upload of
// the caller, so the owner gate of a download treats both alike.
func TestPut_RecordsTheRequestsOwner(t *testing.T) {
	d := memory.New()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-scope-org_id", "org-a", "x-w17-label-org", "ignored"))
	h, err := kvfs.Put(ctx, d, kvfs.Placement{BucketPath: "/exports"}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := kvfs.KeyOwner(h)
	if !ok || owner["org_id"] != "org-a" || len(owner) != 1 {
		t.Errorf("KeyOwner(%q) = %v %v, want {org_id: org-a}", h, owner, ok)
	}
	// No caller → no record, the key is exactly what it was before.
	h2, err := kvfs.Put(context.Background(), d, kvfs.Placement{BucketPath: "/exports"}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kvfs.KeyOwner(h2); ok || strings.Contains(h2, ".o") {
		t.Errorf("a put with no caller recorded an owner: %q", h2)
	}
}
