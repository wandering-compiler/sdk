package composefacts

import (
	"strings"
	"testing"
)

// A marker test answers the same on the extract as on the text it came from.
// That is the whole contract.
func TestExtractKeepsEveryMarkerAnswer(t *testing.T) {
	bodies := []string{
		"",
		"syntax = \"proto3\";\nmessage A {}\n",
		"option (w17.rest_api) = { audience: \"partner\" };\nservice S {}\n",
		"option (w17.admin_api) = {};\noption (w17.private_api) = {};\n",
		"option (w17.rpc_api) = {};\n// option (w17.mcp_api)\n",
	}
	for _, body := range bodies {
		got := string(Extract([]byte(body)))
		for _, m := range Markers {
			if strings.Contains(body, m) != strings.Contains(got, m) {
				t.Errorf("marker %q: text says %v, extract says %v\n  text: %q\n  extract: %q",
					m, strings.Contains(body, m), strings.Contains(got, m), body, got)
			}
		}
		if len(got) > len(strings.Join(Markers, "\n"))+1 {
			t.Errorf("extract is not compact: %q", got)
		}
	}
}

func TestExtractOfAPlainFileIsEmpty(t *testing.T) {
	if got := Extract([]byte("syntax = \"proto3\";\nmessage A { string id = 1; }\n")); got != nil {
		t.Fatalf("Extract = %q, want nil", got)
	}
}
