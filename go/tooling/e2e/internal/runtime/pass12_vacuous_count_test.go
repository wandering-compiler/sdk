package runtime

import (
	"strings"
	"testing"
)

// TestCountMatcher_RefusesAPredicateEveryCountSatisfies — reported by deinvo,
// 2026-09-07.
//
// A count is never negative, so `{matcher: count, op: '>=', value: 0}` holds
// for every possible response: an empty list passes, a full one passes, and
// a field that is not in the response AT ALL passes, because a missing field
// has no elements.
//
// That spelling used to be the generated default for a list endpoint.
// `8141e3ef9` replaced it with the `unwritten` placeholder, which fixes what
// the SCAFFOLD writes — but a skeleton is generate-if-missing, so every file
// created before that keeps it, and nothing stops an author typing it today.
//
// deinvo found 28 in their own suite. Tightening them to `>= 1` turned two
// green cases red, and both were asserting on a field the response does not
// have: an admin page had been repointed at a projection whose list is
// `items`, while the case still named `memberships`. Neither could ever have
// failed, so neither ever reported the drift.
//
// The refusal is deliberately at DECODE time rather than at match time: the
// case must not run at all, the same way `unwritten` does not run. A
// vacuous assertion that executes and passes is worse than one that was
// never written, because it looks finished.
func TestCountMatcher_RefusesAPredicateEveryCountSatisfies(t *testing.T) {
	vacuous := []map[string]any{
		{"matcher": "count", "op": ">=", "value": 0},
		{"matcher": "count", "op": ">=", "value": -1},
		{"matcher": "count", "op": ">", "value": -1},
		{"matcher": "count", "op": "!=", "value": -1},
	}
	for _, m := range vacuous {
		_, err := DecodeMatcher(m)
		if err == nil {
			t.Errorf("%v was accepted, and it holds for every possible response — including one that does not "+
				"contain the field at all", m)
			continue
		}
		// The message has to name both honest alternatives, because which
		// one is right is the question the author actually has to answer.
		for _, want := range []string{">= 1", "== 0"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal for %v does not offer %q as the alternative: %v", m, want, err)
			}
		}
	}
}

// TestCountMatcher_KeepsEveryPredicateThatCanFail — the other direction, and
// the one that matters most: `== 0` is a real assertion (this list must be
// empty) and is exactly what several of deinvo's cases became once the
// vacuous ones were tightened. A refusal that swept those up would have
// taken the useful half with the useless one.
func TestCountMatcher_KeepsEveryPredicateThatCanFail(t *testing.T) {
	fine := []map[string]any{
		{"matcher": "count", "op": "==", "value": 0},
		{"matcher": "count", "op": "<=", "value": 0},
		{"matcher": "count", "op": ">=", "value": 1},
		{"matcher": "count", "op": ">", "value": 0},
		{"matcher": "count", "op": "!=", "value": 0},
		{"matcher": "count", "op": "==", "value": 3},
	}
	for _, m := range fine {
		if _, err := DecodeMatcher(m); err != nil {
			t.Errorf("%v refused, but it can fail: %v", m, err)
		}
	}
}
