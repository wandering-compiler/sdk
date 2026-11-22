package adminspa

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

var exact = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Every version is exact, and the Mantine packages share one: they
// peer-require each other at one exact version, and a set that parts them
// installs two cores.
func TestTheSetIsExact(t *testing.T) {
	s := Deps()
	if s.Node == "" || len(s.Dependencies) == 0 || len(s.DevDependencies) == 0 {
		t.Fatalf("incomplete set: %+v", s)
	}
	mantine := ""
	for _, m := range []map[string]string{s.Dependencies, s.DevDependencies} {
		for name, v := range m {
			if !exact.MatchString(v) {
				t.Errorf("%s: %q is not an exact version", name, v)
			}
			if strings.HasPrefix(name, "@mantine/") {
				if mantine == "" {
					mantine = v
				} else if v != mantine {
					t.Errorf("%s: %s, but another Mantine package is %s", name, v, mantine)
				}
			}
		}
	}
	if _, ok := s.Dependencies["@w17/admin-runtime"]; ok {
		t.Error("the admin runtime is the project's (file:), not part of the set")
	}
}

// Every peer the admin runtime declares is in the set at the peer's major
// (its ranges are caret ranges).
func TestTheSetSatisfiesTheRuntimesPeers(t *testing.T) {
	b, err := os.ReadFile("../../../ts/admin-runtime/package.json")
	if err != nil {
		t.Skipf("the admin runtime's package.json is not beside this module: %v", err)
	}
	var pkg struct {
		Peers map[string]string `json:"peerDependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	s := Deps()
	for name, want := range pkg.Peers {
		got, ok := s.Dependencies[name]
		if !ok {
			t.Errorf("the runtime peer-requires %s %s; the set does not list it", name, want)
			continue
		}
		major := strings.SplitN(strings.TrimPrefix(want, "^"), ".", 2)[0]
		if !strings.HasPrefix(got, major+".") {
			t.Errorf("%s: the set has %s, the runtime wants %s", name, got, want)
		}
	}
}
