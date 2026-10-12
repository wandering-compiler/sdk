package migrate

import (
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A cycle cannot come from an append-only console history, so a directory
// that holds one was written by something else. The walk must refuse it, and
// must terminate doing so rather than follow the links forever.
func TestChainFromTarget_RefusesACycleAndTerminates(t *testing.T) {
	a := &applyfetchpb.Migration{Id: "ts-1", ContentSha256: "aa", PrevContentSha256: "bb"}
	b := &applyfetchpb.Migration{Id: "ts-2", ContentSha256: "bb", PrevContentSha256: "aa"}
	self := &applyfetchpb.Migration{Id: "ts-3", ContentSha256: "cc", PrevContentSha256: "cc"}
	cases := map[string]struct {
		set    []*applyfetchpb.Migration
		target *applyfetchpb.Migration
	}{
		"two-link cycle": {[]*applyfetchpb.Migration{a, b}, b},
		"self link":      {[]*applyfetchpb.Migration{self}, self},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := chainFromTarget(c.set, c.target)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "revisits") {
					t.Fatalf("a cyclic chain was not refused as a revisit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the chain walk did not terminate on a cycle")
			}
		})
	}
}
