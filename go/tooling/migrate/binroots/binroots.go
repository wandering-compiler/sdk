// Package binroots names the argv[1] words a generated w17 binary answers to
// itself, and why each one is taken.
//
// It exists so there is ONE list rather than two that agree today. The
// compiler refuses a project CLI command whose first segment collides with a
// built-in; the binary dispatches on that same word. Those are two ends of one
// rule, in two repositories' worth of code, and a word added at the dispatch
// end without the refusal end is a silent shadowing — the built-in runs and the
// project's own method becomes unreachable with no error anywhere. That is the
// exact bug the refusal was written for, so the refusal must not be able to
// fall behind the thing it describes.
//
// Hence a leaf package with no imports: the parser can depend on it without
// pulling database drivers into the compiler, and the runtime can depend on it
// without pulling the compiler into a deployed binary.
package binroots

// Reserved maps a reserved first segment to the reason it cannot be reused.
// The reason is written for a developer reading a compiler diagnostic, not for
// a maintainer reading this file.
var Reserved = map[string]string{
	"migrate": "a generated binary that owns a database answers `migrate` itself (apply / fetch / rollback / status), " +
		"dispatching on it before any project command is consulted — so a project command of the same name would be " +
		"shadowed silently rather than refused",
	"fixtures": "a generated binary that owns a database answers `fixtures` itself (apply), " +
		"dispatching on it before any project command is consulted — so a project command of the same name would be " +
		"shadowed silently rather than refused",
}

// The reserved words, as constants, so the dispatcher and the tests that check
// it cannot drift from the map by a typo.
const (
	Migrate  = "migrate"
	Fixtures = "fixtures"
)
