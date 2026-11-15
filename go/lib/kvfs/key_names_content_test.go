package kvfs_test

import (
	"context"
	"path"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
)

// TestPut_KeyNamesTheContentOnly: an object's name is its content hash and a
// nonce, whoever the request serves. Recording the caller's data scopes in
// it made the name grow with them — a UUID org + user and a 64-character
// org slug overflowed the 255-byte filename limit on LOCAL_FS, failing
// every upload and business Put of such an organization.
func TestPut_KeyNamesTheContentOnly(t *testing.T) {
	d := local_fs.New(t.TempDir())
	md := metadata.Pairs(
		"x-w17-scope-org_id", "0b6f0c1e-5a3e-4c55-9d6b-2f1d8f1c9a10",
		"x-w17-scope-user_id", "7c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f",
		"x-w17-scope-org_slug", strings.Repeat("northwind-traders-", 3)+"ltd-ab",
		"x-w17-scope-org_permissions", strings.Repeat("documents.read,", 20),
	)
	h, err := kvfs.Put(metadata.NewIncomingContext(context.Background(), md), d, kvfs.Placement{BucketPath: "/attachments"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if name := path.Base(h); !regexp.MustCompile(`^[0-9a-f]{64}-[0-9a-f]{32}$`).MatchString(name) {
		t.Errorf("object name %q (%d bytes): want <sha256 hex>-<nonce hex>", name, len(name))
	}
}
