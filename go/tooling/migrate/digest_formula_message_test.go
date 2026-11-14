package migrate

import (
	"strings"
	"testing"
	"testing/fstest"

	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// Review F3 (T2-5 pass #16): sdk#169 put the required-schema set into the
// digest. A console on the new formula mints digests a binary built against an
// older sdk/go recomputes differently — and that binary's refusal read as an
// integrity failure ("edited", "tampering"), never as "this build is older
// than the digest". The refusal now names the newest formula this build knows
// and the remedy (raise the sdk/go pin), so the NEXT formula change is
// diagnosable from the refusal of every binary built from here on.
func TestMismatchRefusals_NameTheDigestFormula(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id: "0001_init", Connection: "main",
		UpSql: "CREATE TABLE t (id BIGINT);", DownSql: "DROP TABLE t;",
		ContentSha256: strings.Repeat("ab", 32), // a digest no formula of this build produces
	}
	err := WriteMigration(t.TempDir(), m)
	if err == nil {
		t.Fatal("WriteMigration accepted a digest no formula produces")
	}
	for _, want := range []string{DigestFormula, "sdk/go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("fetch-side refusal does not name %q:\n%v", want, err)
		}
	}

	buf, merr := protojson.Marshal(m)
	if merr != nil {
		t.Fatal(merr)
	}
	_, err = loadConnectionMigrations(fstest.MapFS{"main/0001_init.json": {Data: buf}}, "main")
	if err == nil {
		t.Fatal("the loader accepted a digest no formula produces")
	}
	for _, want := range []string{DigestFormula, "sdk/go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("load-side refusal does not name %q:\n%v", want, err)
		}
	}
}
