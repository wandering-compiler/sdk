package runtime

import "testing"

// `${once:name}` is the same value everywhere in one scope.
//
// `seq` and `seq:<name>` both climb on every resolution — naming one
// partitions the counter, it does not share the value. So an e-mail built in
// one step and looked up in another never matched (`worker163@…` created,
// `worker164@…` searched), and the failure read as "no such user" rather than
// as two different values.
func TestOnce_SameValueWithinAScope(t *testing.T) {
	resetProcessSeq()
	run := &Run{}
	sc := run.NewScope()

	a, err := sc.resolve("once:worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b, err := sc.resolve("once:worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if a != b {
		t.Fatalf("once minted twice in one scope: %v vs %v — the create and the lookup would disagree", a, b)
	}
}

// Two names are two values: `once` shares a value, it does not collapse
// distinct identities into one.
func TestOnce_DistinctNamesDiffer(t *testing.T) {
	resetProcessSeq()
	sc := (&Run{}).NewScope()

	worker, _ := sc.resolve("once:worker")
	admin, _ := sc.resolve("once:admin")
	if worker == admin {
		t.Fatalf("two names produced one value (%v) — separate identities would collide", worker)
	}
}

// Two SCOPES must not share, or one test's rows answer another's lookups and
// the suite passes for the wrong reason.
func TestOnce_ScopesDoNotShare(t *testing.T) {
	resetProcessSeq()
	run := &Run{}

	first, _ := run.NewScope().resolve("once:worker")
	second, _ := run.NewScope().resolve("once:worker")
	if first == second {
		t.Fatalf("two scopes shared %v — a second test would reuse the first's identity and hit the unique index", first)
	}
}

// `seq` keeps climbing. It is the per-use uniqueness token and a lot of specs
// lean on that; `once` is an addition, not a redefinition.
func TestSeq_StillIncrementsPerUse(t *testing.T) {
	resetProcessSeq()
	sc := (&Run{}).NewScope()

	a, _ := sc.resolve("seq")
	b, _ := sc.resolve("seq")
	if a == b {
		t.Fatalf("seq stopped incrementing (%v twice) — every spec relying on per-use uniqueness would now collide", a)
	}
}
