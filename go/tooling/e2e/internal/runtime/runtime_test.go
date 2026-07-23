package runtime

import (
	"testing"

	"github.com/google/uuid"
)

// --- generators + interpolation ---------------------------------------

func TestSeqGlobalAcrossScopes(t *testing.T) {
	resetProcessSeq()
	run := NewRun()
	s1 := run.NewScope()
	v1, _ := s1.expandString("${seq}")
	v2, _ := s1.expandString("${seq}")
	s2 := run.NewScope() // new top-level test, fresh captures...
	v3, _ := s2.expandString("${seq}")
	if v1 != 1 || v2 != 2 || v3 != 3 {
		t.Errorf("seq across scopes = %v,%v,%v; want 1,2,3 (global per run)", v1, v2, v3)
	}
}

func TestNamedSeqIndependent(t *testing.T) {
	resetProcessSeq()
	s := NewRun().NewScope()
	a1, _ := s.expandString("${seq:a}")
	b1, _ := s.expandString("${seq:b}")
	a2, _ := s.expandString("${seq:a}")
	if a1 != 1 || b1 != 1 || a2 != 2 {
		t.Errorf("named seq = %v,%v,%v; want 1,1,2", a1, b1, a2)
	}
}

func TestEmbeddedSeqStringifies(t *testing.T) {
	resetProcessSeq()
	s := NewRun().NewScope()
	v, err := s.expandString("user${seq}@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if v != "user1@example.com" {
		t.Errorf("embedded = %v, want user1@example.com", v)
	}
}

func TestRandomUUID(t *testing.T) {
	s := NewRun().NewScope()
	v, err := s.expandString("${random:uuid}")
	if err != nil {
		t.Fatal(err)
	}
	str, ok := v.(string)
	if !ok {
		t.Fatalf("uuid not a string: %T", v)
	}
	if _, err := uuid.Parse(str); err != nil {
		t.Errorf("not a uuid: %q", str)
	}
}

func TestRandomUnknownKind(t *testing.T) {
	s := NewRun().NewScope()
	if _, err := s.expandString("${random:bogus}"); err == nil {
		t.Fatal("want error for unknown random kind")
	}
}

func TestCaptureRefTypedAndNested(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("project.id", "abc-123")
	s.Capture("count", 42)
	s.Capture("project", map[string]any{"name": "demo"})

	// whole-token returns native type
	v, _ := s.expandString("${count}")
	if v != 42 {
		t.Errorf("count = %v (%T), want int 42", v, v)
	}
	// flat dotted key
	v, _ = s.expandString("${project.id}")
	if v != "abc-123" {
		t.Errorf("project.id = %v", v)
	}
	// nested traversal into a captured object
	v, _ = s.expandString("${project.name}")
	if v != "demo" {
		t.Errorf("project.name = %v, want demo", v)
	}
}

func TestUnresolvedRefErrors(t *testing.T) {
	s := NewRun().NewScope()
	if _, err := s.expandString("${nope}"); err == nil {
		t.Fatal("want error for unresolved reference")
	}
}

func TestCapturesLocalToScope(t *testing.T) {
	run := NewRun()
	s1 := run.NewScope()
	s1.Capture("token", "t1")
	s2 := run.NewScope()
	if _, err := s2.expandString("${token}"); err == nil {
		t.Fatal("captures must not leak across scopes")
	}
}

func TestExpandNested(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("pid", "p1")
	in := map[string]any{
		"id":   "${pid}",
		"tags": []any{"a", "${pid}"},
		"meta": map[string]any{"owner": "${pid}"},
	}
	out, err := Expand(in, s)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["id"] != "p1" {
		t.Errorf("id = %v", m["id"])
	}
	if m["tags"].([]any)[1] != "p1" {
		t.Errorf("tags = %v", m["tags"])
	}
	if m["meta"].(map[string]any)["owner"] != "p1" {
		t.Errorf("meta = %v", m["meta"])
	}
}

// --- matchers ----------------------------------------------------------

func match(t *testing.T, spec any, actual any, present bool) error {
	t.Helper()
	m, err := DecodeMatcher(spec)
	if err != nil {
		t.Fatalf("decode %v: %v", spec, err)
	}
	return m.Match(actual, present, NewRun().NewScope())
}

func TestExactMatcherNumericTolerance(t *testing.T) {
	// YAML int 1 vs JSON float64 1.0
	if err := match(t, 1, float64(1), true); err != nil {
		t.Errorf("1 == 1.0 should pass: %v", err)
	}
	if err := match(t, "hello", "hello", true); err != nil {
		t.Errorf("string exact: %v", err)
	}
	if err := match(t, "hello", "world", true); err == nil {
		t.Error("mismatched strings should fail")
	}
}

func TestNotEmptyMatcher(t *testing.T) {
	spec := map[string]any{"matcher": "not_empty"}
	if err := match(t, spec, "x", true); err != nil {
		t.Errorf("present non-zero should pass: %v", err)
	}
	if err := match(t, spec, "", true); err == nil {
		t.Error("present zero should fail not_empty")
	}
	if err := match(t, spec, nil, false); err == nil {
		t.Error("absent should fail not_empty")
	}
}

func TestEmptyMatcher(t *testing.T) {
	spec := map[string]any{"matcher": "empty"}
	if err := match(t, spec, nil, false); err != nil {
		t.Errorf("absent should pass empty: %v", err)
	}
	if err := match(t, spec, 0, true); err != nil {
		t.Errorf("present zero should pass empty: %v", err)
	}
	if err := match(t, spec, "x", true); err == nil {
		t.Error("present non-zero should fail empty")
	}
}

func TestRegexMatcher(t *testing.T) {
	spec := map[string]any{"matcher": "regex", "pattern": "^(queued|running)$"}
	if err := match(t, spec, "queued", true); err != nil {
		t.Errorf("queued should match: %v", err)
	}
	if err := match(t, spec, "done", true); err == nil {
		t.Error("done should not match")
	}
}

func TestCountMatcher(t *testing.T) {
	ge1 := map[string]any{"matcher": "count", "op": ">=", "value": 1}
	if err := match(t, ge1, []any{"a", "b"}, true); err != nil {
		t.Errorf("len 2 >= 1: %v", err)
	}
	if err := match(t, ge1, []any{}, true); err == nil {
		t.Error("len 0 >= 1 should fail")
	}
	eq3 := map[string]any{"matcher": "count", "op": "==", "value": 3}
	if err := match(t, eq3, []any{1, 2, 3}, true); err != nil {
		t.Errorf("len 3 == 3: %v", err)
	}
	// absent collection counts as zero
	if err := match(t, map[string]any{"matcher": "count", "op": "==", "value": 0}, nil, false); err != nil {
		t.Errorf("absent == 0: %v", err)
	}
	if err := match(t, eq3, "not-a-collection", true); err == nil {
		t.Error("non-collection should fail count")
	}
}

func TestEqMatcher(t *testing.T) {
	// escape hatch: assert a literal that looks like a keyword
	spec := map[string]any{"matcher": "eq", "value": "not_empty"}
	if err := match(t, spec, "not_empty", true); err != nil {
		t.Errorf("eq literal: %v", err)
	}
}

func TestCaptureMatcherBindsAndAsserts(t *testing.T) {
	scope := NewRun().NewScope()
	// { capture: build_id, match: { matcher: not_empty } }
	spec := map[string]any{"capture": "build_id", "match": map[string]any{"matcher": "not_empty"}}
	m, err := DecodeMatcher(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Match("bid-7", true, scope); err != nil {
		t.Fatalf("capture+match: %v", err)
	}
	if scope.captures["build_id"] != "bid-7" {
		t.Errorf("not captured: %v", scope.captures)
	}
	// inner match still fails on empty
	if err := m.Match("", true, scope); err == nil {
		t.Error("capture inner not_empty should fail on empty")
	}
}

func TestExactMatcherInterpolatesExpected(t *testing.T) {
	scope := NewRun().NewScope()
	scope.Capture("build_id", "b-42")
	m, _ := DecodeMatcher("${build_id}")
	if err := m.Match("b-42", true, scope); err != nil {
		t.Errorf("interpolated expected should match: %v", err)
	}
	if err := m.Match("other", true, scope); err == nil {
		t.Error("should fail when actual differs from captured expected")
	}
}

func TestMatchExpectIntegration(t *testing.T) {
	scope := NewRun().NewScope()
	expect := map[string]any{
		"build_id": map[string]any{"capture": "build_id"},
		"status":   map[string]any{"matcher": "regex", "pattern": "^(queued|running)$"},
		"tasks":    map[string]any{"matcher": "count", "op": ">=", "value": 1},
		"name":     "demo",
	}
	actual := map[string]any{
		"build_id": "bid-1",
		"status":   "queued",
		"tasks":    []any{map[string]any{"id": 1}},
		"name":     "demo",
	}
	if err := MatchExpect(expect, actual, scope); err != nil {
		t.Fatalf("MatchExpect: %v", err)
	}
	if scope.captures["build_id"] != "bid-1" {
		t.Errorf("capture not bound: %v", scope.captures)
	}

	bad := map[string]any{"build_id": "x", "status": "done", "tasks": []any{}, "name": "demo"}
	if err := MatchExpect(expect, bad, scope); err == nil {
		t.Error("expected failure on bad status")
	}
}

// TestMatchExpectNestedPath — a dotted expect key resolves into nested
// objects + array indices, so a capture can bind a value buried in a
// list (the clickable-id flow: ListUsers → users.0.id → CreateTask).
func TestMatchExpectNestedPath(t *testing.T) {
	scope := NewRun().NewScope()
	expect := map[string]any{
		"users.0.id":    map[string]any{"capture": "user.id"},
		"users.0.email": map[string]any{"matcher": "not_empty"},
	}
	actual := map[string]any{
		"users": []any{
			map[string]any{"id": 7, "email": "a@b.c"},
			map[string]any{"id": 9, "email": "d@e.f"},
		},
	}
	if err := MatchExpect(expect, actual, scope); err != nil {
		t.Fatalf("MatchExpect nested: %v", err)
	}
	if got := scope.captures["user.id"]; !looseEqual(got, 7) {
		t.Errorf("captured user.id = %v, want 7", got)
	}
	// out-of-range / missing path → absent → not_empty fails
	if err := MatchExpect(map[string]any{"users.5.id": map[string]any{"matcher": "not_empty"}}, actual, scope); err == nil {
		t.Error("out-of-range index should be treated as absent (not_empty must fail)")
	}
	// a literal top-level key containing a dot still wins
	lit := map[string]any{"a.b": map[string]any{"matcher": "eq", "value": "x"}}
	if err := MatchExpect(lit, map[string]any{"a.b": "x"}, scope); err != nil {
		t.Errorf("literal dotted key should resolve directly: %v", err)
	}
}

func TestDecodeUnknownMatcher(t *testing.T) {
	if _, err := DecodeMatcher(map[string]any{"matcher": "bogus"}); err == nil {
		t.Fatal("want error for unknown matcher kind")
	}
}
