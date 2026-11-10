package kvfsgrpc_test

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/kvfsgrpc"
	filespb "github.com/wandering-compiler/sdk/go/pb/common/files"
)

// A handle is what Put returned: hash-sharded hex segments. One with a
// dot-segment — the local driver's staging area `.w17-staging`, `.` or
// `..` — names no stored file and MUST be refused before the driver is
// asked, so a caller cannot read or delete another request's in-flight
// upload.
func TestDotSegmentHandlesAreRefused(t *testing.T) {
	f := newFixture(t)
	staged := filepath.Join(f.root, ".w17-staging", "w17-upload-1")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{".w17-staging/w17-upload-1", "note-exports/../.w17-staging/w17-upload-1", "./x", ".hidden"} {
		_, err := f.client.Stat(ctx(t), &filespb.StatRequest{Connection: "app-exports", Handle: h})
		wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
		_, _, err = read(t, f.client, "app-exports", h)
		wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
		_, err = f.client.Delete(ctx(t), &filespb.DeleteRequest{Connection: "app-exports", Handle: h})
		wantRefusal(t, err, codes.InvalidArgument, kvfsgrpc.ReasonBadRequest, nil)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("the staged upload is gone: %v", err)
	}
}
