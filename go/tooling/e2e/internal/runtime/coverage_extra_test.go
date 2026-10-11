package runtime

import (
	"strings"
	"testing"
)

// --- context.go: Get / Captures / Seed / lookup / asStringMap ---------

func TestScopeGet(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("auth.token", "tok-1")
	if v, ok := s.Get("auth.token"); !ok || v != "tok-1" {
		t.Errorf("Get(auth.token) = %v,%v; want tok-1,true", v, ok)
	}
	if v, ok := s.Get("missing"); ok || v != nil {
		t.Errorf("Get(missing) = %v,%v; want nil,false", v, ok)
	}
}

func TestScopeCapturesCopy(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("a", 1)
	s.Capture("b", "two")
	cp := s.Captures()
	if cp["a"] != 1 || cp["b"] != "two" {
		t.Fatalf("Captures = %v", cp)
	}
	// mutating the copy must not touch the live scope
	cp["a"] = 99
	if v, _ := s.Get("a"); v != 1 {
		t.Errorf("live scope mutated via copy: a = %v", v)
	}
}

func TestScopeSeed(t *testing.T) {
	s := NewRun().NewScope()
	s.Seed(map[string]any{"token": "t", "id": 7})
	if v, _ := s.Get("token"); v != "t" {
		t.Errorf("seeded token = %v", v)
	}
	if v, _ := s.Get("id"); v != 7 {
		t.Errorf("seeded id = %v", v)
	}
}

func TestScopeSeedRoundTrip(t *testing.T) {
	src := NewRun().NewScope()
	src.Capture("auth.token", "abc")
	src.Capture("obj", map[string]any{"id": "x"})

	dst := NewRun().NewScope()
	dst.Seed(src.Captures())
	if v, ok := dst.Get("auth.token"); !ok || v != "abc" {
		t.Errorf("round-trip token = %v,%v", v, ok)
	}
	// nested traversal still works through a seeded object
	v, err := dst.expandString("${obj.id}")
	if err != nil || v != "x" {
		t.Errorf("seeded nested = %v,%v; want x", v, err)
	}
}

func TestLookupNestedThroughNonMap(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("a", "scalar") // intermediate is not a map
	if _, ok := s.lookup("a.b"); ok {
		t.Error("lookup through a scalar should be unresolved")
	}
}

func TestLookupDottedHeadMissing(t *testing.T) {
	s := NewRun().NewScope()
	// nothing bound; dotted name → head "x" not in captures
	if _, ok := s.lookup("x.y"); ok {
		t.Error("lookup with missing head should be false")
	}
}

func TestLookupNestedMissingTailKey(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("obj", map[string]any{"present": 1})
	if _, ok := s.lookup("obj.absent"); ok {
		t.Error("missing nested key should be false")
	}
}

func TestAsStringMapVariants(t *testing.T) {
	// map[any]any with string keys normalises
	out, ok := asStringMap(map[any]any{"k": 1, "j": "v"})
	if !ok || out["k"] != 1 || out["j"] != "v" {
		t.Errorf("map[any]any normalise = %v,%v", out, ok)
	}
	// map[any]any with a non-string key is rejected
	if _, ok := asStringMap(map[any]any{1: "v"}); ok {
		t.Error("non-string key should be rejected")
	}
	// already-string-keyed map passes through
	if _, ok := asStringMap(map[string]any{"a": 1}); !ok {
		t.Error("map[string]any should be accepted")
	}
	// non-map is rejected
	if _, ok := asStringMap("nope"); ok {
		t.Error("non-map should be rejected")
	}
}

// --- generator.go: int + string kinds (+ randHex) ---------------------

func TestGenerateRandomKinds(t *testing.T) {
	iv, err := generateRandom("int")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := iv.(int)
	if !ok || n < 0 {
		t.Errorf("random:int = %v (%T), want non-negative int", iv, iv)
	}
	sv, err := generateRandom("string")
	if err != nil {
		t.Fatal(err)
	}
	hs, ok := sv.(string)
	if !ok || len(hs) != 8 {
		t.Errorf("random:string = %q (%T), want 8 hex chars", sv, sv)
	}
	for _, c := range hs {
		if !strings.ContainsRune(hexDigits, c) {
			t.Errorf("random:string has non-hex char %q", c)
		}
	}
}

// --- interpolate.go: map[any]any, error propagation, embedded errors --

func TestExpandMapAnyAny(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("pid", "p1")
	out, err := Expand(map[any]any{"id": "${pid}"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["id"] != "p1" {
		t.Errorf("map[any]any expand = %v", out)
	}
}

func TestExpandMapAnyAnyNonStringKey(t *testing.T) {
	s := NewRun().NewScope()
	if _, err := Expand(map[any]any{1: "x"}, s); err == nil {
		t.Error("non-string map key should error")
	}
}

func TestExpandErrorPropagation(t *testing.T) {
	s := NewRun().NewScope()
	// error inside a map[string]any value
	if _, err := Expand(map[string]any{"k": "${nope}"}, s); err == nil {
		t.Error("map value resolve error should propagate")
	}
	// error inside a slice element
	if _, err := Expand([]any{"ok", "${nope}"}, s); err == nil {
		t.Error("slice element resolve error should propagate")
	}
	// passthrough scalar (default arm)
	out, err := Expand(42, s)
	if err != nil || out != 42 {
		t.Errorf("scalar passthrough = %v,%v", out, err)
	}
}

func TestExpandStringEmbeddedError(t *testing.T) {
	s := NewRun().NewScope()
	if _, err := s.expandString("prefix-${nope}-suffix"); err == nil {
		t.Error("embedded unresolved token should error")
	}
}

// --- matcher.go: resolveField default, DecodeMatcher errors, ops ------

func TestResolveFieldTraversesIntoScalar(t *testing.T) {
	// path descends into a scalar leaf → absent
	if _, ok := resolveField(map[string]any{"a": "scalar"}, "a.b"); ok {
		t.Error("descending into a scalar should be absent")
	}
	// non-numeric index into a slice → absent
	actual := map[string]any{"list": []any{1, 2}}
	if _, ok := resolveField(actual, "list.x"); ok {
		t.Error("non-numeric slice index should be absent")
	}
	// negative index → absent
	if _, ok := resolveField(actual, "list.-1"); ok {
		t.Error("negative slice index should be absent")
	}
	// valid nested slice index → present
	if v, ok := resolveField(actual, "list.1"); !ok || !looseEqual(v, 2) {
		t.Errorf("list.1 = %v,%v; want 2,true", v, ok)
	}
	// plain key (no dot) not present → absent
	if _, ok := resolveField(map[string]any{}, "missing"); ok {
		t.Error("missing dotless key should be absent")
	}
	// dotted path with a missing map segment → absent
	if _, ok := resolveField(map[string]any{"a": map[string]any{}}, "a.b"); ok {
		t.Error("missing nested map segment should be absent")
	}
}

func TestDecodeMatcherNestedObject(t *testing.T) {
	// A mapping with neither `capture` nor `matcher` → a nested matcher
	// that descends field-by-field (PARTIAL match).
	scope := NewRun().NewScope()
	m, err := DecodeMatcher(map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Match(map[string]any{"foo": "bar"}, true, scope); err != nil {
		t.Errorf("nested object per-field match: %v", err)
	}
	if err := m.Match(map[string]any{"foo": "other"}, true, scope); err == nil {
		t.Error("nested object should fail when a listed field mismatches")
	}
	// PARTIAL: an extra field in the actual object is ignored.
	if err := m.Match(map[string]any{"foo": "bar", "extra": 1}, true, scope); err != nil {
		t.Errorf("nested object should ignore extra actual fields (partial): %v", err)
	}
	// Absent / non-object actual → error.
	if err := m.Match(nil, false, scope); err == nil {
		t.Error("nested object should fail when the field is absent")
	}
	if err := m.Match("scalar", true, scope); err == nil {
		t.Error("nested object should fail when actual is not an object")
	}
}

// TestNestedCapture is the gap-2 regression: a capture directive nested
// inside an expect map (not a dotted key) must bind, at any depth.
func TestNestedCapture(t *testing.T) {
	scope := NewRun().NewScope()
	expect := map[string]any{
		"account": map[string]any{"id": map[string]any{"capture": "account_id"}},
		"wallet":  map[string]any{"id": map[string]any{"capture": "wallet_id"}},
	}
	actual := map[string]any{
		"account": map[string]any{"id": "acc-1"},
		"wallet":  map[string]any{"id": "wal-9"},
	}
	if err := MatchExpect(expect, actual, scope); err != nil {
		t.Fatalf("nested capture match: %v", err)
	}
	caps := scope.Captures()
	if caps["account_id"] != "acc-1" {
		t.Errorf("account_id = %v, want acc-1", caps["account_id"])
	}
	if caps["wallet_id"] != "wal-9" {
		t.Errorf("wallet_id = %v, want wal-9", caps["wallet_id"])
	}
	// The dotted-key form must remain equivalent.
	scope2 := NewRun().NewScope()
	if err := MatchExpect(map[string]any{"account.id": map[string]any{"capture": "aid"}}, actual, scope2); err != nil {
		t.Fatalf("dotted capture match: %v", err)
	}
	if scope2.Captures()["aid"] != "acc-1" {
		t.Errorf("dotted account.id capture = %v, want acc-1", scope2.Captures()["aid"])
	}
}

// TestExactObjectMatcher — whole-object exact equality (incl. rejecting
// extra fields) is available via the explicit `{matcher: eq}` form.
func TestExactObjectMatcher(t *testing.T) {
	scope := NewRun().NewScope()
	m, err := DecodeMatcher(map[string]any{
		"matcher": "eq",
		"value":   map[string]any{"foo": "bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Match(map[string]any{"foo": "bar"}, true, scope); err != nil {
		t.Errorf("eq exact object match: %v", err)
	}
	// Exact match rejects an extra field (unlike the partial nested form).
	if err := m.Match(map[string]any{"foo": "bar", "extra": 1}, true, scope); err == nil {
		t.Error("eq should reject an object with extra fields")
	}
}

func TestMatchExpectBadSpec(t *testing.T) {
	scope := NewRun().NewScope()
	// capture with empty name → DecodeMatcher error surfaced by MatchExpect
	expect := map[string]any{"f": map[string]any{"capture": ""}}
	if err := MatchExpect(expect, map[string]any{"f": "v"}, scope); err == nil {
		t.Error("bad matcher spec should surface from MatchExpect")
	}
}

func TestDecodeMatcherErrors(t *testing.T) {
	cases := []struct {
		name string
		spec any
	}{
		{"capture non-string", map[string]any{"capture": 7}},
		{"capture empty", map[string]any{"capture": ""}},
		{"capture inner bad", map[string]any{"capture": "x", "match": map[string]any{"matcher": "bogus"}}},
		{"regex missing pattern", map[string]any{"matcher": "regex"}},
		{"regex bad pattern", map[string]any{"matcher": "regex", "pattern": "("}},
		{"count non-number", map[string]any{"matcher": "count", "value": "x"}},
		{"count unknown op", map[string]any{"matcher": "count", "op": "~=", "value": 1}},
		{"matcher kind not a string", map[string]any{"matcher": 7}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeMatcher(c.spec); err == nil {
				t.Errorf("%s: want decode error", c.name)
			}
		})
	}
}

func TestCountDefaultOpAndAllOps(t *testing.T) {
	scope := NewRun().NewScope()
	// op omitted → defaults to "=="
	m, err := DecodeMatcher(map[string]any{"matcher": "count", "value": 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Match([]any{1, 2}, true, scope); err != nil {
		t.Errorf("default op == : %v", err)
	}
	// exercise every comparison op, pass + fail
	ops := []struct {
		op         string
		got        []any
		want       float64
		shouldPass bool
	}{
		{"!=", []any{1}, 2, true},
		{"!=", []any{1, 2}, 2, false},
		{"<=", []any{1, 2}, 2, true},
		{"<=", []any{1, 2, 3}, 2, false},
		{">", []any{1, 2, 3}, 2, true},
		{">", []any{1}, 2, false},
		{"<", []any{1}, 2, true},
		{"<", []any{1, 2, 3}, 2, false},
		{">=", []any{1, 2}, 2, true},
	}
	for _, c := range ops {
		mm, err := DecodeMatcher(map[string]any{"matcher": "count", "op": c.op, "value": c.want})
		if err != nil {
			t.Fatalf("decode %s: %v", c.op, err)
		}
		err = mm.Match(c.got, true, scope)
		if c.shouldPass && err != nil {
			t.Errorf("len %d %s %v should pass: %v", len(c.got), c.op, c.want, err)
		}
		if !c.shouldPass && err == nil {
			t.Errorf("len %d %s %v should fail", len(c.got), c.op, c.want)
		}
	}
}

func TestRegexMatcherAbsent(t *testing.T) {
	if err := match(t, map[string]any{"matcher": "regex", "pattern": "x"}, nil, false); err == nil {
		t.Error("absent field should fail regex")
	}
}

func TestExactMatcherAbsentVsNil(t *testing.T) {
	// expected non-nil but field absent → error
	if err := match(t, "want", nil, false); err == nil {
		t.Error("absent field with non-nil expected should fail")
	}
	// expected nil and absent → passes (looseEqual nil==nil after expand)
	if err := match(t, nil, nil, false); err != nil {
		t.Errorf("nil expected + absent should pass: %v", err)
	}
	// expected interpolation fails (unresolved capture) → error from Expand
	if err := match(t, "${nope}", "x", true); err == nil {
		t.Error("unresolvable expected token should error")
	}
}

// --- isZero / toFloat across kinds ------------------------------------

func TestIsZeroKinds(t *testing.T) {
	var nilPtr *int
	p := new(int)
	cases := []struct {
		v    any
		zero bool
	}{
		{nil, true},
		{"", true},
		{"x", false},
		{false, true},
		{true, false},
		{int(0), true},
		{int64(5), false},
		{int8(0), true},
		{uint(0), true},
		{uint32(3), false},
		{float64(0), true},
		{float32(1.5), false},
		{[]any{}, true},
		{[]any{1}, false},
		{map[string]any{}, true},
		{map[string]any{"a": 1}, false},
		{nilPtr, true},
		{p, false},
		{struct{ X int }{}, false}, // unhandled kind → default false
	}
	for i, c := range cases {
		if got := isZero(c.v); got != c.zero {
			t.Errorf("case %d isZero(%#v) = %v, want %v", i, c.v, got, c.zero)
		}
	}
}

func TestToFloatKinds(t *testing.T) {
	cases := []struct {
		v  any
		ok bool
		f  float64
	}{
		{int(1), true, 1},
		{int32(2), true, 2},
		{int64(3), true, 3},
		{uint(4), true, 4},
		{uint32(5), true, 5},
		{uint64(6), true, 6},
		{float32(7), true, 7},
		{float64(8), true, 8},
		{"nope", false, 0},
		{nil, false, 0},
	}
	for _, c := range cases {
		f, ok := toFloat(c.v)
		if ok != c.ok || (ok && f != c.f) {
			t.Errorf("toFloat(%#v) = %v,%v; want %v,%v", c.v, f, ok, c.f, c.ok)
		}
	}
}
