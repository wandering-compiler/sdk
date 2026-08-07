package runner

import (
	"errors"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// A scenario can only ever observe matchCallError through a full stack,
// where a refusal is expensive to arrange and a WRONG refusal is
// expensive to arrange twice. These cover the discriminations the e2e
// suite cannot: that the code is compared at all, and that a transport
// failure is not mistaken for a guard firing.

func scope() *runtime.Scope { return runtime.NewRun().NewScope() }

func TestMatchCallError_AcceptsTheDeclaredRefusal(t *testing.T) {
	err := matchCallError(
		&ExpectError{Code: "PERMISSION_DENIED", Message: map[string]any{"matcher": "regex", "pattern": "CountTasksPerCategory"}},
		&CallError{Status: 403, Code: "PERMISSION_DENIED", Message: "forbidden: missing permission tasks.TaskLookupQuery.CountTasksPerCategory"},
		scope(),
	)
	if err != nil {
		t.Fatalf("declared refusal rejected: %v", err)
	}
}

func TestMatchCallError_RejectsADifferentCode(t *testing.T) {
	// Without this comparison every deny case would pass on ANY refusal —
	// including a 404 from a mistyped route, which is the shape most
	// likely to be mistaken for a working gate.
	err := matchCallError(
		&ExpectError{Code: "PERMISSION_DENIED"},
		&CallError{Status: 404, Code: "NOT_FOUND", Message: "no such route"},
		scope(),
	)
	if err == nil {
		t.Fatal("a NOT_FOUND must not satisfy an expected PERMISSION_DENIED")
	}
	if !strings.Contains(err.Error(), "NOT_FOUND") || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("message names neither side of the mismatch: %v", err)
	}
}

func TestMatchCallError_RejectsAWrongMessage(t *testing.T) {
	// The message is where the permission NAME lives, so a right-code /
	// wrong-permission refusal is the one that would otherwise read as
	// proof of the gate under test.
	err := matchCallError(
		&ExpectError{Code: "PERMISSION_DENIED", Message: map[string]any{"matcher": "regex", "pattern": "CountTasksPerCategory"}},
		&CallError{Status: 403, Code: "PERMISSION_DENIED", Message: "forbidden: missing permission tasks.TaskQuery.GetTask"},
		scope(),
	)
	if err == nil {
		t.Fatal("a refusal naming a DIFFERENT permission must not satisfy the contract")
	}
}

func TestMatchCallError_RejectsSuccess(t *testing.T) {
	err := matchCallError(&ExpectError{Code: "PERMISSION_DENIED"}, nil, scope())
	if err == nil {
		t.Fatal("a successful call must fail a refusal contract")
	}
	if !strings.Contains(err.Error(), "SUCCEEDED") {
		t.Errorf("the leak case needs to say the guard let the caller through, got: %v", err)
	}
}

func TestMatchCallError_RejectsATransportFailure(t *testing.T) {
	// Connection refused is not a refusal. Treating it as one would turn
	// a dead surface into a green authorization suite.
	err := matchCallError(&ExpectError{Code: "PERMISSION_DENIED"}, errors.New("dial tcp: connection refused"), scope())
	if err == nil {
		t.Fatal("a transport failure must not satisfy a refusal contract")
	}
	if !strings.Contains(err.Error(), "before the surface answered") {
		t.Errorf("message does not distinguish a transport failure from a refusal: %v", err)
	}
}

func TestNewCallError_DecodesTheCanonicalEnvelope(t *testing.T) {
	e := newCallError("rest GET /x", 403, []byte(`{"error":{"code":"PERMISSION_DENIED","message":"forbidden: missing permission a.B.C"}}`))
	if e.Code != "PERMISSION_DENIED" || !strings.Contains(e.Message, "a.B.C") {
		t.Fatalf("envelope not decoded: %+v", e)
	}
	// A body that is NOT the envelope leaves the code empty rather than
	// inventing one — an assertion then fails on the missing code, which
	// is the honest outcome: the surface did not answer in the contract.
	plain := newCallError("rest GET /x", 502, []byte("<html>bad gateway</html>"))
	if plain.Code != "" {
		t.Errorf("non-envelope body produced a code: %q", plain.Code)
	}
	if !strings.Contains(plain.Error(), "bad gateway") {
		t.Errorf("non-envelope body must still be readable: %v", plain)
	}
}

// The `details` half. A whole class of refusals says the same thing in
// `message` — every request-validation failure answers "validation
// failed" — so without reading the per-field details, six different
// rules produce six identical assertions.

func validationRefusal() *CallError {
	return &CallError{
		Status: 400, Code: "INVALID_ARGUMENT", Message: "validation failed",
		Details: []CallErrorDetail{
			{Field: "min_priority", Code: "LT_VIOLATION", Message: "the lowest priority in the window must be below the highest one"},
			{Field: "reason", Code: "REQUIRED_VIOLATION", Message: "a report that is not narrowed to one title needs a reason on record"},
		},
	}
}

func TestMatchCallError_AcceptsADeclaredDetail(t *testing.T) {
	err := matchCallError(
		&ExpectError{Code: "INVALID_ARGUMENT", Details: []ExpectErrorDetail{
			{Field: "min_priority", Code: "LT_VIOLATION"},
		}},
		validationRefusal(), scope(),
	)
	if err != nil {
		t.Fatalf("a detail the refusal carries was rejected: %v", err)
	}
}

func TestMatchCallError_ExtraDetailsAreAllowed(t *testing.T) {
	// One request routinely trips several rules. Demanding the exact set
	// would make every case depend on the others' fixtures — change one
	// input and unrelated cases go red.
	err := matchCallError(
		&ExpectError{Code: "INVALID_ARGUMENT", Details: []ExpectErrorDetail{
			{Code: "REQUIRED_VIOLATION"},
		}},
		validationRefusal(), scope(),
	)
	if err != nil {
		t.Fatalf("an unlisted sibling detail must not fail the match: %v", err)
	}
}

func TestMatchCallError_RejectsADetailFromAnotherRule(t *testing.T) {
	// THE case this exists for: the request was refused, with the right
	// code, by a DIFFERENT rule than the one under test.
	err := matchCallError(
		&ExpectError{Code: "INVALID_ARGUMENT", Details: []ExpectErrorDetail{
			{Field: "title_is", Code: "NOT_CONTAINS_VIOLATION"},
		}},
		validationRefusal(), scope(),
	)
	if err == nil {
		t.Fatal("a refusal from another rule must not satisfy the contract")
	}
	// The diagnosis is "which rules DID fire" — without it the author
	// re-runs the suite to find out.
	if !strings.Contains(err.Error(), "LT_VIOLATION") {
		t.Errorf("the failure does not report the details that came back: %v", err)
	}
}

func TestMatchCallError_RejectsADetaillessRefusal(t *testing.T) {
	// A surface that stopped forwarding details would otherwise make
	// every detail assertion vacuous in the direction that matters.
	err := matchCallError(
		&ExpectError{Code: "INVALID_ARGUMENT", Details: []ExpectErrorDetail{{Code: "LT_VIOLATION"}}},
		&CallError{Status: 400, Code: "INVALID_ARGUMENT", Message: "validation failed"},
		scope(),
	)
	if err == nil {
		t.Fatal("a refusal carrying no details must not satisfy a detail contract")
	}
	if !strings.Contains(err.Error(), "NO per-field details") {
		t.Errorf("the failure does not say the details were missing: %v", err)
	}
}

func TestMatchCallError_MatchesADetailMessageWithAMatcher(t *testing.T) {
	err := matchCallError(
		&ExpectError{Code: "INVALID_ARGUMENT", Details: []ExpectErrorDetail{
			{Field: "min_priority", Message: map[string]any{"matcher": "regex", "pattern": "below the highest"}},
		}},
		validationRefusal(), scope(),
	)
	if err != nil {
		t.Fatalf("a matcher over the detail message was rejected: %v", err)
	}
}

func TestNewCallError_DecodesDetails(t *testing.T) {
	// The envelope half: details that never leave the JSON are details
	// no assertion can reach.
	ce := newCallError("rest GET /tasks/in-priority-range", 400, []byte(
		`{"error":{"code":"INVALID_ARGUMENT","message":"validation failed","details":[{"field":"min_priority","code":"LT_VIOLATION","message":"too low"}]}}`))
	if len(ce.Details) != 1 {
		t.Fatalf("details = %#v, want one entry", ce.Details)
	}
	if ce.Details[0].Field != "min_priority" || ce.Details[0].Code != "LT_VIOLATION" {
		t.Errorf("detail decoded wrong: %#v", ce.Details[0])
	}
}
