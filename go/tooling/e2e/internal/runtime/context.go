// Package runtime is the execution-side half of the generated e2e
// tests: it expands request inputs (captures + generators), and
// matches responses against the structured matcher vocabulary. It
// ships inside the gateway bundle binary (imported by the `test`
// subcommand the codegen emits); the runner (sdk/go/tooling/e2e/runner)
// drives it.
//
// Two scopes, per the resolved design:
//
//   - A Run is created once per `<binary> test` invocation. It owns
//     the `${seq}` counters, which are monotonic across the WHOLE
//     run so generated ids never collide between tests.
//   - A Scope is created per top-level test and shared with that
//     test's pre/post chain. It owns the captures (`${name}`,
//     `auth.token`), which reset between top-level tests.
package runtime

import "sync"

// processSeq backs the `${seq}` counters. It is PROCESS-global, not
// per-Run, deliberately: the generated Go-test harness calls
// RunScenario once per top-level test (Test_app_rest, Test_app_admin,
// …), each with a fresh Run. A per-Run counter would restart at 1 in
// every test function, so two scenarios sharing one backing DB would
// mint colliding ids (`e2e-app-1` created by the admin suite, then
// re-minted by the rest suite → unique violation). A process-global
// counter realizes the documented contract — "monotonic across the
// WHOLE run so generated ids never collide between tests" — for the
// per-test-function harness too. Cross-process isolation (a second
// `go test` invocation) is the DB-reset's job, not the counter's.
var (
	processSeqMu sync.Mutex
	processSeq   = map[string]int{}
)

// Run is the per-invocation state of one `<binary> test` run. Run
// holds no per-instance counter state anymore — the `${seq}`
// counters live in process-global storage (see processSeq) so they
// survive across the fresh Run each RunScenario call creates.
type Run struct{}

// NewRun creates a fresh run handle. The sequence counters it drives
// are process-global, so a new Run keeps climbing from where the
// last one left off.
func NewRun() *Run {
	return &Run{}
}

// nextSeq advances the named counter and returns its new value.
// The empty name backs the bare `${seq}`; `${seq:users}` uses
// "users". Counters start at 1. Process-global + mutex-guarded so
// concurrent scenarios (should a harness ever parallelize) don't
// race the map.
func (r *Run) nextSeq(name string) int {
	processSeqMu.Lock()
	defer processSeqMu.Unlock()
	processSeq[name]++
	return processSeq[name]
}

// resetProcessSeq clears the process-global counters. Test-only seam
// (the counters intentionally have no production reset — within a
// real test binary they must climb monotonically); unit tests that
// assert absolute seq values call it to isolate from sibling tests.
func resetProcessSeq() {
	processSeqMu.Lock()
	defer processSeqMu.Unlock()
	processSeq = map[string]int{}
}

// NewScope opens a capture scope for one top-level test. Captures
// are local to the scope; sequence counters stay shared with the
// run, so `${seq}` keeps climbing across tests.
func (r *Run) NewScope() *Scope {
	return &Scope{run: r, captures: map[string]any{}}
}

// Scope is the capture context for one top-level test and its
// pre/post chain. Captures bound by a `{capture: <var>}` matcher in
// any step are visible to every later step in the same scope.
type Scope struct {
	run      *Run
	captures map[string]any
}

// Capture binds a value to a name for later `${name}` resolution.
// Dotted names (`auth.token`, `project.id`) are stored as flat keys;
// resolution tries the flat key first, then nested traversal.
func (s *Scope) Capture(name string, value any) {
	s.captures[name] = value
}

// Get resolves a capture reference for callers outside the package
// (the runner reads the reserved `auth.token` to inject auth).
func (s *Scope) Get(name string) (any, bool) { return s.lookup(name) }

// Captures returns a shallow copy of the scope's bound captures. The
// stress engine uses it to seed each load worker's own scope from the
// values a once-only setup phase captured (the auth token, a created
// row's id) — so concurrent workers share the same upstream state
// (race-condition checks hammer the SAME id) without sharing the live
// map. Values are copied by reference (a captured id string / object is
// immutable in practice), the map itself is fresh.
func (s *Scope) Captures() map[string]any {
	out := make(map[string]any, len(s.captures))
	for k, v := range s.captures {
		out[k] = v
	}
	return out
}

// Seed binds a copied capture set into this scope (the inverse of
// Captures). Used to prime a worker scope from the setup phase before
// the load loop runs.
func (s *Scope) Seed(captures map[string]any) {
	for k, v := range captures {
		s.captures[k] = v
	}
}

// lookup resolves a capture reference. It tries the exact flat key
// first (so a `{capture: project.id}` binding is found directly),
// then falls back to walking dotted segments through nested maps
// (so capturing `project` as an object lets `${project.id}` reach
// into it). The bool is false when the name resolves to nothing.
func (s *Scope) lookup(name string) (any, bool) {
	if v, ok := s.captures[name]; ok {
		return v, true
	}
	// nested traversal: split on '.', walk maps
	cur, rest, found := splitFirst(name)
	v, ok := s.captures[cur]
	if !ok || !found {
		return nil, false
	}
	for rest != "" {
		m, isMap := asStringMap(v)
		if !isMap {
			return nil, false
		}
		cur, rest, _ = splitFirst(rest)
		v, ok = m[cur]
		if !ok {
			return nil, false
		}
	}
	return v, true
}

func splitFirst(s string) (head, tail string, hadDot bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// asStringMap normalises the two map shapes the runtime sees —
// map[string]any (json/yaml decode) and map[any]any (legacy yaml) —
// into a string-keyed map.
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = val
		}
		return out, true
	}
	return nil, false
}
