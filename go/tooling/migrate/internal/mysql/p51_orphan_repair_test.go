package mysql

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// T1-1 pass #51 review R2 — a key the plan drops and re-adds is the plain
// ADD on MySQL (no NOT VALID), so an orphan an earlier tolerant add kept
// fails it with 1452, which names only the constraint. The failure names the
// repair; every other error passes through untouched.
func TestWithOrphanRepairNames1452Only(t *testing.T) {
	fk := fmt.Errorf("exec: %w", &gomysql.MySQLError{Number: 1452, Message: "Cannot add or update a child row"})
	got := withOrphanRepair(fk)
	if !errors.Is(got, fk) || !strings.Contains(got.Error(), "re-create the missing parent rows") {
		t.Errorf("1452 does not name the repair (or lost its cause): %v", got)
	}
	other := &gomysql.MySQLError{Number: 1146, Message: "Table doesn't exist"}
	if got := withOrphanRepair(other); got != error(other) {
		t.Errorf("another error was rewritten: %v", got)
	}
}
