package migratecli

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/healthcheck"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/binroots"
)

// Every reserved root is answered by the dispatch chain a generated main
// runs (health first, then this package), and none of them is the generic
// refusal of a word nobody knows: `migrate`, `fixtures` and `health` run,
// and the retired `schema` is refused by name with the way out.
func TestDispatchChain_AnswersEveryReservedRoot(t *testing.T) {
	for root := range binroots.Reserved {
		argv := []string{root, "--help"}
		handled, err := healthcheck.Dispatch(context.Background(), argv, healthcheck.Options{Out: &strings.Builder{}})
		if !handled {
			handled, err = Dispatch(context.Background(), argv, Options{Out: &strings.Builder{}})
		}
		if !handled {
			t.Errorf("reserved root %q fell through to the server", root)
			continue
		}
		if root == binroots.Schema {
			if err == nil || !strings.Contains(err.Error(), "no `schema` command") || !strings.Contains(err.Error(), "migrate") {
				t.Errorf("`schema` = %v, want the refusal that names it and points at migrate", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("reserved root %q was refused: %v", root, err)
		}
	}
}
