// Package adminspa is the dependency set of a generated admin SPA: the npm
// packages its package.json lists beside @w17/admin-runtime, and the Node
// image it builds on.
//
// One set for every project on this SDK, so it has one source, deps.json,
// read by both sides that need it: the compiler, which writes each admin
// bundle's spa/package.json from it, and the SDK's image workflow, which
// installs exactly this set into ghcr.io/wandering-compiler/sdk-admin-spa
// (images/admin-spa). A bundle built from that image runs `vite build`
// without downloading anything, and every build of one SDK release gets the
// same tree — the transitive dependencies included, which a package.json
// alone leaves to the registry of the day.
//
// Every version is exact (TestTheSetIsExact). The admin runtime is not in
// the set: the project vendors it (file:), and it declares the packages here
// as peers, which the set must satisfy (TestTheSetSatisfiesTheRuntimesPeers).
package adminspa

import (
	_ "embed"
	"encoding/json"
	"sort"
)

//go:embed deps.json
var depsJSON []byte

// Set is deps.json.
type Set struct {
	// Node is the node image tag the SPA builds on (`node:<Node>`), and the
	// one the image is built from.
	Node            string            `json:"node"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// Deps is the set. It panics on a deps.json that does not parse: the file is
// this package's own, and a test reads it.
func Deps() Set {
	var s Set
	if err := json.Unmarshal(depsJSON, &s); err != nil {
		panic("adminspa: deps.json: " + err.Error())
	}
	return s
}

// Names returns the keys of m, sorted: package.json order.
func Names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
