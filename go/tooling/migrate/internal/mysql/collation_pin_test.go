package mysql_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// The applier carries the session's collation pin, and like the runtime it
// leaves an operator's own `collation` alone: the pin is a SET after the
// handshake and would overrule it.
func TestURLToDriverDSN_CollationPinAndOperatorChoice(t *testing.T) {
	bare, err := mysql.URLToDriverDSN("mysql://u:p@db:3306/app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bare, "collation_connection=utf8mb4_0900_as_cs") {
		t.Errorf("bare DSN lacks the collation pin: %q", bare)
	}
	chosen, err := mysql.URLToDriverDSN("mysql://u:p@db:3306/app?collation=utf8mb4_bin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(chosen, "collation_connection") || !strings.Contains(chosen, "collation=utf8mb4_bin") {
		t.Errorf("an operator's collation was overruled: %q", chosen)
	}
}
