package runtime

import (
	"math"
	"strings"
	"testing"
)

// `distinct` counts like `count`, and an absent list counts zero distinct
// values — so `{matcher: distinct, field: x, op: '>=', value: 0}` held for
// every response, a missing field included, while the same predicate under
// `count` was refused at decode.
func TestDistinctMatcher_RefusesAPredicateEveryCountSatisfies(t *testing.T) {
	for _, m := range []map[string]any{
		{"matcher": "distinct", "field": "org_id", "op": ">=", "value": 0},
		{"matcher": "distinct", "field": "org_id", "op": "!=", "value": -1},
		{"matcher": "distinct", "field": "org_id", "op": ">", "value": -1},
	} {
		if _, err := DecodeMatcher(m); err == nil || !strings.Contains(err.Error(), "holds for every possible count") {
			t.Errorf("%v: err = %v, want it refused as vacuous", m, err)
		}
	}
	for _, m := range []map[string]any{
		{"matcher": "distinct", "field": "org_id", "op": "==", "value": 1},
		{"matcher": "distinct", "field": "org_id", "op": ">=", "value": 2},
		{"matcher": "distinct", "field": "org_id", "op": "==", "value": 0},
	} {
		if _, err := DecodeMatcher(m); err != nil {
			t.Errorf("%v: refused: %v", m, err)
		}
	}
}

// A NaN or infinite operand makes a comparison that cannot fail (`!= NaN`,
// `>= -Inf`) or cannot pass; neither says anything about the value.
func TestNumericMatchers_RefuseANonFiniteValue(t *testing.T) {
	for _, kind := range []string{"num", "count", "distinct"} {
		for _, v := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
			m := map[string]any{"matcher": kind, "op": "!=", "value": v}
			if kind == "distinct" {
				m["field"] = "org_id"
			}
			if _, err := DecodeMatcher(m); err == nil || !strings.Contains(err.Error(), "finite") {
				t.Errorf("%s value %v: err = %v, want a non-finite value refused", kind, v, err)
			}
		}
	}
}
