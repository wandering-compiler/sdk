package fileref_test

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/fileref"
)

// TestReadable_FollowsTheRecord: the download gate decides by the row that
// references a file, read through the caller's own data scopes — not by
// who stored the file, and not by anything recorded in its key.
func TestReadable_FollowsTheRecord(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "refs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, org_id TEXT, owner_id TEXT, file TEXT)`,
		// alice's note in org A; staff of org A attached its file.
		`INSERT INTO notes (org_id, owner_id, file) VALUES ('org-a', 'alice', 'attachments/aa-staff-attached')`,
		// a file stored before any gate existed: an old-format key.
		`INSERT INTO notes (org_id, owner_id, file) VALUES ('org-a', 'alice', 'attachments/legacy-0000')`,
		// a row with no organization (NULL) and one with an empty one.
		`INSERT INTO notes (org_id, owner_id, file) VALUES (NULL, NULL, 'attachments/no-org')`,
		`INSERT INTO notes (org_id, owner_id, file) VALUES ('', '', 'attachments/empty-org')`,
		`CREATE TABLE docs (id INTEGER PRIMARY KEY, file TEXT)`,
		`INSERT INTO docs (file) VALUES ('attachments/public-doc')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	srv := fileref.NewServer(
		fileref.Ref{
			Bucket: "attachments", DB: db,
			Query: `SELECT 1 FROM notes WHERE file = ? AND org_id = ? AND (owner_id = ? OR ? IS NULL) LIMIT 1`,
			Args:  []fileref.Arg{fileref.KeyArg(), fileref.ScopeArg("org_id"), fileref.OwnerArg("user_id"), fileref.OwnerArg("user_id")},
		},
		fileref.Ref{
			Bucket: "attachments", DB: db,
			Query: `SELECT 1 FROM docs WHERE file = ? LIMIT 1`,
			Args:  []fileref.Arg{fileref.KeyArg()},
		},
	)

	// Through a real gRPC hop: the registration and the client the
	// generated code uses.
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fileref.RegisterW17FileRefsServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	caller := func(kv ...string) context.Context {
		return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(kv...))
	}
	alice := caller("x-w17-scope-org_id", "org-a", "x-w17-scope-user_id", "alice")
	bob := caller("x-w17-scope-org_id", "org-a", "x-w17-scope-user_id", "bob")
	bobOrgRange := caller("x-w17-scope-org_id", "org-a", "x-w17-scope-user_id", "bob", "x-w17-data-scope", "org")
	otherOrg := caller("x-w17-scope-org_id", "org-b", "x-w17-scope-user_id", "alice")
	noOrg := caller("x-w17-scope-user_id", "alice")
	emptyOrg := caller("x-w17-scope-org_id", "", "x-w17-scope-user_id", "")
	nobody := context.Background()
	twoOrgs := caller("x-w17-scope-org_id", "org-a", "x-w17-scope-org_id", "org-b", "x-w17-scope-user_id", "alice")

	cases := []struct {
		name string
		ctx  context.Context
		key  string
		want bool
	}{
		{"staff attached it to alice's record: alice", alice, "attachments/aa-staff-attached", true},
		{"same organization, another owner, owner range", bob, "attachments/aa-staff-attached", false},
		{"same organization, organization range", bobOrgRange, "attachments/aa-staff-attached", true},
		{"another organization", otherOrg, "attachments/aa-staff-attached", false},
		{"a caller with no organization", noOrg, "attachments/aa-staff-attached", false},
		{"a pre-existing (old-key) file: its record's owner", alice, "attachments/legacy-0000", true},
		{"no-scope caller vs no-scope record (NULL)", noOrg, "attachments/no-org", false},
		{"no-scope caller vs no-scope record (NULL), no metadata at all", nobody, "attachments/no-org", false},
		{"empty-scope caller vs empty-scope record", emptyOrg, "attachments/empty-org", false},
		{"scoped caller vs no-scope record", alice, "attachments/no-org", false},
		{"an ambiguous scope (two values)", twoOrgs, "attachments/aa-staff-attached", false},
		{"an unscoped model's record: any caller", nobody, "attachments/public-doc", true},
		{"a key no record references", alice, "attachments/unknown", false},
		{"a key outside the bucket", alice, "elsewhere/aa-staff-attached", false},
	}
	for _, c := range cases {
		got, err := fileref.Readable(c.ctx, conn, c.key)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: Readable(%q) = %v, want %v", c.name, c.key, got, c.want)
		}
	}
}
