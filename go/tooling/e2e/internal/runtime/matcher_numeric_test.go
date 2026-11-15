package runtime

import (
	"strings"
	"testing"
)

// `num` fills the hole `count` leaves: count refuses a scalar on
// purpose, so an aggregate response had no way to say "at least one"
// except `not_empty`, which happens to reject zero and says so nowhere.
func TestNumMatcher(t *testing.T) {
	spec := map[string]any{"matcher": "num", "op": ">=", "value": 1}
	if err := match(t, spec, float64(14), true); err != nil {
		t.Errorf("14 >= 1 rejected: %v", err)
	}
	if err := match(t, spec, float64(0), true); err == nil {
		t.Error("0 >= 1 must fail — this is the case the matcher exists for")
	}
	// proto3 serialises int64 / uint64 as STRINGS, so the most common
	// aggregate field in this compiler's output arrives quoted. A matcher
	// that refused it would be useless — the first version of this one
	// was, and failed on the first aggregate it was pointed at.
	if err := match(t, spec, "14", true); err != nil {
		t.Errorf("int64-as-string rejected: %v", err)
	}
	if err := match(t, spec, "0", true); err == nil {
		t.Error(`"0" must fail >= 1 — quoting must not smuggle a zero past the comparison`)
	}
	if err := match(t, spec, "abc", true); err == nil {
		t.Error("a non-numeric string must not satisfy a numeric comparison")
	}
	if err := match(t, spec, nil, false); err == nil {
		t.Error("an absent field must fail rather than compare as zero")
	}
}

// `distinct` is the direct form of "these rows came from more than one
// X" — the scope-bypass assertion that previously needed a caller
// arranged to own zero rows plus a second case establishing that zero.
func TestDistinctMatcher(t *testing.T) {
	rows := []any{
		map[string]any{"id": 1.0, "org_id": "alpha"},
		map[string]any{"id": 2.0, "org_id": "alpha"},
		map[string]any{"id": 3.0, "org_id": "beta"},
	}
	spec := map[string]any{"matcher": "distinct", "field": "org_id", "op": ">=", "value": 2}
	if err := match(t, spec, rows, true); err != nil {
		t.Errorf("two organizations rejected: %v", err)
	}

	oneOrg := []any{
		map[string]any{"id": 1.0, "org_id": "alpha"},
		map[string]any{"id": 2.0, "org_id": "alpha"},
	}
	err := match(t, spec, oneOrg, true)
	if err == nil {
		t.Fatal("a single-organization result must fail >= 2 — that is a scope that held")
	}
	if !strings.Contains(err.Error(), "org_id") {
		t.Errorf("failure does not name the field: %v", err)
	}
}

func TestDistinctMatcher_TypoInFieldIsLoud(t *testing.T) {
	// Reporting zero distinct values for a misspelled field would pass
	// any `== 0` assertion and read as a real result.
	rows := []any{map[string]any{"org_id": "alpha"}}
	err := match(t, map[string]any{"matcher": "distinct", "field": "org", "op": "==", "value": 0}, rows, true)
	if err == nil {
		t.Fatal("a field no element carries must be an error, not a count of zero")
	}
}

func TestDistinctMatcher_RequiresField(t *testing.T) {
	if _, err := DecodeMatcher(map[string]any{"matcher": "distinct", "op": ">=", "value": 2}); err == nil {
		t.Fatal("distinct without `field` must be refused at build time")
	}
}

// An absent list is an empty one: no distinct values. `== 0` accepts it
// and `>= 1` refuses it, as `count` does.
func TestDistinctMatcher_AbsentListIsEmpty(t *testing.T) {
	if err := match(t, map[string]any{"matcher": "distinct", "field": "org_id", "op": "==", "value": 0}, nil, false); err != nil {
		t.Errorf("absent list failed distinct == 0: %v", err)
	}
	if err := match(t, map[string]any{"matcher": "distinct", "field": "org_id", "op": ">=", "value": 1}, nil, false); err == nil {
		t.Error("absent list passed distinct >= 1")
	}
}

// TestNumMatcher_Int64PastFloatPrecision: an int64 travels as a decimal
// string, and `num` compared it through float64 — above 2^53 two
// different values round to the same float, so `== 9007199254740992`
// held for a response of "9007199254740993" and `!=` failed on it. The
// comparison is exact for integers and decimal strings.
func TestNumMatcher_Int64PastFloatPrecision(t *testing.T) {
	eq := map[string]any{"matcher": "num", "op": "==", "value": int64(9007199254740992)}
	if err := match(t, eq, "9007199254740993", true); err == nil {
		t.Error(`"9007199254740993" == 9007199254740992 held — compared through float64`)
	}
	if err := match(t, eq, "9007199254740992", true); err != nil {
		t.Errorf("the equal int64 was refused: %v", err)
	}
	ne := map[string]any{"matcher": "num", "op": "!=", "value": int64(9007199254740992)}
	if err := match(t, ne, "9007199254740993", true); err != nil {
		t.Errorf("two different int64s compared equal: %v", err)
	}
	gt := map[string]any{"matcher": "num", "op": ">", "value": int64(9223372036854775806)}
	if err := match(t, gt, "9223372036854775807", true); err != nil {
		t.Errorf("max int64 > max-1 failed: %v", err)
	}
	half := map[string]any{"matcher": "num", "op": "==", "value": 0.5}
	if err := match(t, half, "0.5", true); err != nil {
		t.Errorf("a decimal string lost its fraction: %v", err)
	}
}
