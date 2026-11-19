package goversion

import (
	"os"
	"strings"
	"testing"
)

// The constants and the repository's go.version name one line: the Makefile
// and the scripts read the file, the generators these constants.
func TestMinorMatchesGoVersionFile(t *testing.T) {
	b, err := os.ReadFile("../../../go.version")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != Minor {
		t.Errorf("go.version says %q, goversion.Minor %q", got, Minor)
	}
}
