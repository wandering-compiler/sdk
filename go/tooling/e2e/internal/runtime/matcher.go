package runtime

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Matcher asserts one response field. `present` distinguishes an
// absent field from a present zero-value (load-bearing for
// empty/not_empty). `scope` lets a matcher interpolate an expected
// value (`${capture}`) or bind one (`{capture: …}`).
type Matcher interface {
	Match(actual any, present bool, scope *Scope) error
}

// MatchExpect runs every field matcher in an `expect` mapping against
// a decoded response object. It is the runner-facing entry point:
// the runner JSON/structured-decodes the response into a string-keyed
// map and calls this. Matchers see the field as (value, present).
func MatchExpect(expect map[string]any, actual map[string]any, scope *Scope) error {
	for field, spec := range expect {
		m, err := DecodeMatcher(spec)
		if err != nil {
			return fmt.Errorf("expect %q: %w", field, err)
		}
		val, present := resolveField(actual, field)
		if err := m.Match(val, present, scope); err != nil {
			return fmt.Errorf("expect %q: %w", field, err)
		}
	}
	return nil
}

// resolveField looks up an expect key in the decoded response. A plain
// key is a direct top-level lookup; a dotted key is a path into nested
// objects + arrays (`users.0.id`, `paging.total`) so a matcher — most
// usefully `{capture: …}` — can bind a value buried inside a list or
// sub-object the API returns. A literal top-level key wins over path
// interpretation (direct hit short-circuits), so a real key containing
// a dot still resolves. Numeric segments index slices; everything else
// indexes maps. A missing segment yields (nil, false), which matchers
// read as "field absent".
func resolveField(actual map[string]any, field string) (any, bool) {
	if v, ok := actual[field]; ok {
		return v, true
	}
	if !strings.Contains(field, ".") {
		return nil, false
	}
	var cur any = actual
	for _, seg := range strings.Split(field, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// DecodeMatcher turns one parsed `expect` value into a Matcher:
//
//   - a mapping with `capture` → a CaptureMatcher (optionally
//     wrapping a `match:` sub-matcher);
//   - a mapping with `matcher` → the named structured matcher;
//   - any other mapping → a NESTED matcher that descends into the
//     actual object field-by-field, applying each sub-key as its own
//     matcher (so `{account: {id: {capture: x}}}` binds x, and
//     per-field matchers/captures work at any depth);
//   - a scalar → exact equality.
//
// A bare nested map is a PARTIAL match — extra fields in the actual
// object are ignored. Use `{matcher: eq, value: {...}}` for a
// whole-object exact-equality assertion.
func DecodeMatcher(spec any) (Matcher, error) {
	m, isMap := asStringMap(spec)
	if !isMap {
		return exactMatcher{expected: spec}, nil
	}
	if v, ok := m["capture"]; ok {
		varName, ok := v.(string)
		if !ok || varName == "" {
			return nil, fmt.Errorf("capture: want a non-empty variable name, got %v", v)
		}
		cm := captureMatcher{varName: varName}
		if inner, ok := m["match"]; ok {
			im, err := DecodeMatcher(inner)
			if err != nil {
				return nil, fmt.Errorf("capture %q match: %w", varName, err)
			}
			cm.inner = im
		}
		return cm, nil
	}
	kind, ok := m["matcher"]
	if !ok {
		return nestedMatcher{fields: m}, nil // recurse per-field
	}
	ks, _ := kind.(string)
	switch ks {
	case "not_empty":
		return notEmptyMatcher{}, nil
	case "empty":
		return emptyMatcher{}, nil
	case "eq":
		return exactMatcher{expected: m["value"]}, nil
	case "regex":
		pat, _ := m["pattern"].(string)
		if pat == "" {
			return nil, fmt.Errorf("regex: missing `pattern`")
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("regex: bad pattern %q: %w", pat, err)
		}
		return regexMatcher{re: re}, nil
	case "count":
		op, _ := m["op"].(string)
		if op == "" {
			op = "=="
		}
		val, ok := toFloat(m["value"])
		if !ok {
			return nil, fmt.Errorf("count: `value` must be a number, got %v", m["value"])
		}
		if !validOp(op) {
			return nil, fmt.Errorf("count: unknown op %q (want one of == != >= <= > <)", op)
		}
		return countMatcher{op: op, value: val}, nil
	default:
		return nil, fmt.Errorf("unknown matcher kind %q", ks)
	}
}

// exactMatcher compares the actual value against an expected literal,
// after interpolating the expected (it may reference a capture, e.g.
// `last_build_id: ${build_id}`).
type exactMatcher struct{ expected any }

func (m exactMatcher) Match(actual any, present bool, scope *Scope) error {
	exp, err := Expand(m.expected, scope)
	if err != nil {
		return err
	}
	if !present && exp != nil {
		return fmt.Errorf("expected %v, field absent", exp)
	}
	if !looseEqual(actual, exp) {
		return fmt.Errorf("expected %v, got %v", exp, actual)
	}
	return nil
}

type notEmptyMatcher struct{}

func (notEmptyMatcher) Match(actual any, present bool, _ *Scope) error {
	if !present || isZero(actual) {
		return fmt.Errorf("expected non-empty, got %v (present=%v)", actual, present)
	}
	return nil
}

type emptyMatcher struct{}

func (emptyMatcher) Match(actual any, present bool, _ *Scope) error {
	if present && !isZero(actual) {
		return fmt.Errorf("expected empty, got %v", actual)
	}
	return nil
}

type regexMatcher struct{ re *regexp.Regexp }

func (m regexMatcher) Match(actual any, present bool, _ *Scope) error {
	if !present {
		return fmt.Errorf("regex %q: field absent", m.re.String())
	}
	s := fmt.Sprint(actual)
	if !m.re.MatchString(s) {
		return fmt.Errorf("regex %q: no match for %q", m.re.String(), s)
	}
	return nil
}

type countMatcher struct {
	op    string
	value float64
}

func (m countMatcher) Match(actual any, present bool, _ *Scope) error {
	if !present {
		// absent collection counts as zero
		return compareCount(0, m.op, m.value)
	}
	n, ok := collectionLen(actual)
	if !ok {
		return fmt.Errorf("count: field is not a collection (%T)", actual)
	}
	return compareCount(float64(n), m.op, m.value)
}

// captureMatcher binds the actual value to a scope variable, then —
// if a `match:` sub-matcher was declared — also asserts it.
type captureMatcher struct {
	varName string
	inner   Matcher
}

func (m captureMatcher) Match(actual any, present bool, scope *Scope) error {
	if scope != nil {
		scope.Capture(m.varName, actual)
	}
	if m.inner != nil {
		return m.inner.Match(actual, present, scope)
	}
	return nil
}

// nestedMatcher matches a nested object field-by-field. Each key in
// `fields` is decoded into its own matcher and applied against the
// corresponding sub-field of the actual object, reusing the full
// MatchExpect engine — so dotted keys, captures, and named matchers all
// work at any depth (`{account: {id: {capture: id}}}` binds id;
// `{account: {id.value: {matcher: not_empty}}}` descends further). The
// match is PARTIAL: keys absent from `fields` are not asserted. For a
// strict whole-object comparison use `{matcher: eq, value: {...}}`,
// which routes through exactMatcher's deep equality.
type nestedMatcher struct{ fields map[string]any }

func (m nestedMatcher) Match(actual any, present bool, scope *Scope) error {
	if !present {
		return fmt.Errorf("expected nested object, field absent")
	}
	am, ok := asStringMap(actual)
	if !ok {
		return fmt.Errorf("expected nested object, got %T", actual)
	}
	return MatchExpect(m.fields, am, scope)
}

// --- helpers -----------------------------------------------------------

func validOp(op string) bool {
	switch op {
	case "==", "!=", ">=", "<=", ">", "<":
		return true
	}
	return false
}

func compareCount(got float64, op string, want float64) error {
	ok := false
	switch op {
	case "==":
		ok = got == want
	case "!=":
		ok = got != want
	case ">=":
		ok = got >= want
	case "<=":
		ok = got <= want
	case ">":
		ok = got > want
	case "<":
		ok = got < want
	}
	if !ok {
		return fmt.Errorf("count %v %s %v failed", got, op, want)
	}
	return nil
}

func collectionLen(v any) (int, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Len(), true
	}
	return 0, false
}

// isZero reports the "empty" predicate shared by empty/not_empty: nil,
// the zero numeric/bool/string, or an empty collection.
func isZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// looseEqual compares two values tolerant of the int-vs-float skew
// between YAML-parsed expectations and JSON-parsed responses. Numbers
// compare as float64; everything else falls back to deep equality
// (strings/bools decode to the same Go type on both sides).
func looseEqual(a, b any) bool {
	if fa, oka := toFloat(a); oka {
		if fb, okb := toFloat(b); okb {
			return fa == fb
		}
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
