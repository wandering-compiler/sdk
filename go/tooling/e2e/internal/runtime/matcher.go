package runtime

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
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
		if err := rejectStrayKeys(m, "capture", "match"); err != nil {
			return nil, err
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
	// Once `matcher` is present the key set is fully determined, so a key
	// outside it is an authoring mistake rather than a shape to interpret.
	// Ignoring it silently WEAKENED assertions: in a YAML flow mapping the
	// comma is a separator, so `{matcher: eq, value: Backend engineer,
	// Prague}` binds `value` to "Backend engineer" and lands "Prague" as a
	// stray key — the case still runs and still passes, against half the
	// string its author wrote. See rejectStrayKeys.
	if err := strayKeysForKind(m, ks); err != nil {
		return nil, err
	}
	switch ks {
	case "unwritten":
		return unwrittenMatcher{}, nil
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
	case "num":
		op, val, err := opAndValue(m, "num")
		if err != nil {
			return nil, err
		}
		return numMatcher{op: op, value: val}, nil
	case "distinct":
		field, _ := m["field"].(string)
		if field == "" {
			return nil, fmt.Errorf("distinct: missing `field` (which field's distinct values to count)")
		}
		op, val, err := opAndValue(m, "distinct")
		if err != nil {
			return nil, err
		}
		return distinctMatcher{field: field, op: op, value: val}, nil
	default:
		return nil, fmt.Errorf("unknown matcher kind %q", ks)
	}
}

// strayKeysForKind rejects keys that a matcher of this kind has no slot
// for. Unknown KINDS fall through — `DecodeMatcher`'s default arm reports
// those, and reporting the keys of a matcher nobody recognises first
// would bury the real problem.
func strayKeysForKind(m map[string]any, kind string) error {
	switch kind {
	case "unwritten", "not_empty", "empty":
		return rejectStrayKeys(m, "matcher")
	case "eq":
		return rejectStrayKeys(m, "matcher", "value")
	case "regex":
		return rejectStrayKeys(m, "matcher", "pattern")
	case "count", "num":
		return rejectStrayKeys(m, "matcher", "op", "value")
	case "distinct":
		return rejectStrayKeys(m, "matcher", "field", "op", "value")
	}
	return nil
}

// rejectStrayKeys reports any key of `m` outside `allowed`.
//
// It exists because the failure it catches is INVISIBLE: an ignored key
// leaves a case that runs, passes, and asserts less than it reads as
// asserting. The overwhelmingly likely cause is the YAML flow-mapping
// comma — `{matcher: eq, value: a, b}` is three entries, not two — so
// the message says so rather than only naming the key.
func rejectStrayKeys(m map[string]any, allowed ...string) error {
	var stray []string
	for k := range m {
		if !slices.Contains(allowed, k) {
			stray = append(stray, k)
		}
	}
	if len(stray) == 0 {
		return nil
	}
	sort.Strings(stray) // deterministic message across map iteration order
	return fmt.Errorf("unexpected key(s) %s; this matcher takes only %s.\n"+
		"If the value was meant to contain a comma, QUOTE it: inside a YAML flow mapping "+
		"`{...}` the comma separates entries, so `value: a, b` binds `value` to \"a\" and "+
		"makes \"b\" a key of its own — the assertion then silently checks half of what it says",
		strings.Join(stray, ", "), strings.Join(allowed, ", "))
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
		// When both sides RENDER the same, the mismatch is the type — a
		// quoted "5" against the number 5 (looseEqual coerces numeric
		// types, never string↔number). Naming the types is the whole
		// diagnosis; without it the message reads "expected 5, got 5".
		if fmt.Sprint(exp) == fmt.Sprint(actual) {
			return fmt.Errorf("expected %v (%T), got %v (%T)", exp, exp, actual, actual)
		}
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
		return compareNum("count", 0, m.op, m.value)
	}
	n, ok := collectionLen(actual)
	if !ok {
		return fmt.Errorf("count: field is not a collection (%T)", actual)
	}
	return compareNum("count", float64(n), m.op, m.value)
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

// compareNum is the shared numeric predicate behind `count`, `num` and
// `distinct`. `label` names WHAT was compared, because "3 >= 2 failed"
// with no subject is unreadable in a suite where three different
// matchers can produce it.
func compareNum(label string, got float64, op string, want float64) error {
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
		return fmt.Errorf("%s %v %s %v failed", label, got, op, want)
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
	case reflect.Pointer, reflect.Interface:
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

// opAndValue reads the shared `op` / `value` pair off a comparison
// matcher, defaulting the operator to equality.
func opAndValue(m map[string]any, kind string) (string, float64, error) {
	op, _ := m["op"].(string)
	if op == "" {
		op = "=="
	}
	if !validOp(op) {
		return "", 0, fmt.Errorf("%s: unknown op %q (want one of == != >= <= > <)", kind, op)
	}
	val, ok := toFloat(m["value"])
	if !ok {
		return "", 0, fmt.Errorf("%s: `value` must be a number, got %v", kind, m["value"])
	}
	return op, val, nil
}

// numMatcher compares a SCALAR numerically. `count` deliberately refuses
// a scalar (it counts elements), which left an aggregate response like
// `{total: 14}` with no way to say "at least one" — the workaround was
// `not_empty`, which happens to reject zero but says so only in a
// comment.
type numMatcher struct {
	op    string
	value float64
}

func (m numMatcher) Match(actual any, present bool, _ *Scope) error {
	if !present {
		return fmt.Errorf("num: field absent")
	}
	got, ok := numericValue(actual)
	if !ok {
		return fmt.Errorf("num: field is not a number (%T: %v)", actual, actual)
	}
	return compareNum("value", got, m.op, m.value)
}

// numericValue coerces a response field to a number, INCLUDING a numeric
// string.
//
// That is not laxness: proto3's JSON mapping serialises int64 / uint64
// as strings (they exceed what a JSON number safely holds), so `int64
// total = 1` arrives as `"14"`. A matcher that refused it would be
// useless on the most common numeric field type this compiler emits —
// which is exactly how the first version of this matcher failed, on the
// first aggregate it was pointed at.
//
// Deliberately NOT folded into toFloat, which backs `eq`'s looseEqual:
// there, string-vs-number is a real type mismatch worth reporting, and
// its comment says so. Here the string IS the number, by the wire spec.
func numericValue(v any) (float64, bool) {
	if f, ok := toFloat(v); ok {
		return f, true
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// distinctMatcher counts how many DISTINCT values of `field` appear
// across a collection, and compares that count.
//
// It exists for the assertion "these rows came from more than one X",
// which no combination of the other matchers can express: `contains`
// would have to name the foreign value, and a scenario that reads across
// a scope by definition does not know which values it is about to see.
// Proving a scope bypass previously needed a caller arranged to own zero
// rows, plus a second case establishing that zero — state standing in
// for an assertion.
type distinctMatcher struct {
	field string
	op    string
	value float64
}

func (m distinctMatcher) Match(actual any, present bool, _ *Scope) error {
	if !present {
		return compareNum("distinct "+m.field, 0, m.op, m.value)
	}
	rv := reflect.ValueOf(actual)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return fmt.Errorf("distinct: field is not a list (%T)", actual)
	}
	seen := map[string]bool{}
	for i := 0; i < rv.Len(); i++ {
		el, ok := rv.Index(i).Interface().(map[string]any)
		if !ok {
			return fmt.Errorf("distinct: element %d is not a message (%T)", i, rv.Index(i).Interface())
		}
		v, ok := el[m.field]
		if !ok {
			// A typo in `field` would otherwise report zero distinct
			// values, which passes any `== 0` assertion and reads as a
			// meaningful result.
			return fmt.Errorf("distinct: element %d has no field %q", i, m.field)
		}
		seen[fmt.Sprint(v)] = true
	}
	return compareNum("distinct "+m.field, float64(len(seen)), m.op, m.value)
}

// unwrittenMatcher always fails. It is what a GENERATED e2e skeleton carries
// where its assertion belongs, so an endpoint cannot look covered until
// somebody writes what the response should actually say.
//
// It replaced `{matcher: count, op: ">=", value: 0}` — a count is never
// negative, so that assertion was true of every possible response including
// an empty one. A scaffold like that does not merely under-test: it reports
// coverage. Three separate defects shipped behind one in a single week, one
// of them a list endpoint that answered nothing for a fortnight while its
// case stayed green.
//
// Writing a deliberately weak assertion is still available — `{matcher:
// count, op: ">=", value: 0}` typed by hand does exactly what it used to.
// The difference is that it is then a choice recorded in the author's file,
// not a default they never saw.
type unwrittenMatcher struct{}

func (unwrittenMatcher) Match(actual any, present bool, _ *Scope) error {
	return fmt.Errorf("this assertion is still the generated placeholder — the step ran and its response was not checked\n"+
		"  got: %v (present=%v)\n"+
		"  fix: replace `{matcher: unwritten}` with what the response must actually contain "+
		"(`{matcher: count, op: '==', value: N}`, `{matcher: eq, value: ...}`, …). "+
		"To accept anything on purpose, say so: `{matcher: count, op: '>=', value: 0}`", actual, present)
}
