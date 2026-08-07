package datamigrate_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestTransformVM_TopLevelInfiniteLoop_Bounded is the filegen-sec-2
// regression guard. The compile thread runs the script's top level
// (FileOptions enables While/TopLevelControl). A top-level `while True`
// must hit the step limit and ERROR — not hang the host process. If the
// compile-thread step limit regresses, this test hangs instead of
// failing, which is the symptom we're guarding against.
func TestTransformVM_TopLevelInfiniteLoop_Bounded(t *testing.T) {
	_, err := datamigrate.NewTransformVM(`
while True:
    pass

def transform(value):
    return value
`, "starlark")
	if err == nil {
		t.Fatal("expected a step-limit error for a top-level infinite loop")
	}
	if !strings.Contains(err.Error(), "compile script") {
		t.Errorf("error should come from the bounded compile step, got: %v", err)
	}
}

// TestTransformVM_BytesPassthrough — script that returns the
// input verbatim is treated as a no-op (byte-equal in/out
// short-circuits to changed=false).
func TestTransformVM_BytesPassthrough(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    return value
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	out, changed, err := vm.Apply([]byte("hello"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changed {
		t.Errorf("byte-equal in/out should be changed=false; got %q", out)
	}
}

// TestTransformVM_ReturnsNoneAsNoop — explicit None return
// signals "skip this entry"; treated as changed=false.
func TestTransformVM_ReturnsNoneAsNoop(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    return None
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	out, changed, err := vm.Apply([]byte("hello"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changed || out != nil {
		t.Errorf("None return should yield (nil, false, nil); got out=%v changed=%v", out, changed)
	}
}

// TestTransformVM_JSONModule — the predeclared json module
// supports decode + encode so JSON-encoded bodies are
// transformable in script.
func TestTransformVM_JSONModule(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    doc = json.decode(str(value))
    doc["full_name"] = doc["first"] + " " + doc["last"]
    return bytes(json.encode(doc))
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	in := []byte(`{"first":"Jane","last":"Doe"}`)
	out, changed, err := vm.Apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(string(out), `"full_name":"Jane Doe"`) {
		t.Errorf("expected full_name in output, got: %s", out)
	}
	if !strings.Contains(string(out), `"first":"Jane"`) {
		t.Errorf("first field lost: %s", out)
	}
}

// TestTransformVM_EmptyValue — zero-byte input short-circuits
// to (nil, false, nil); script never runs.
func TestTransformVM_EmptyValue(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    fail("should not be called on empty input")
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	out, changed, err := vm.Apply(nil)
	if err != nil || changed || out != nil {
		t.Errorf("expected (nil, false, nil); got out=%v changed=%v err=%v", out, changed, err)
	}
}

// TestNewTransformVM_RejectsMissingFunction — script must
// define a `transform` callable; missing top-level surfaces
// at compile time.
func TestNewTransformVM_RejectsMissingFunction(t *testing.T) {
	_, err := datamigrate.NewTransformVM(`x = 1`, "starlark")
	if err == nil {
		t.Fatal("expected error for missing transform function")
	}
	if !strings.Contains(err.Error(), "transform") {
		t.Errorf("expected transform-required error, got: %v", err)
	}
}

// TestNewTransformVM_RejectsNonCallable — script defines
// `transform` as a non-callable value (e.g. a constant);
// compile-time refusal.
func TestNewTransformVM_RejectsNonCallable(t *testing.T) {
	_, err := datamigrate.NewTransformVM(`transform = 42`, "starlark")
	if err == nil {
		t.Fatal("expected error for non-callable transform")
	}
	if !strings.Contains(err.Error(), "not callable") {
		t.Errorf("expected not-callable error, got: %v", err)
	}
}

// TestNewTransformVM_RejectsEmptyScript — empty body short-
// circuits before parsing.
func TestNewTransformVM_RejectsEmptyScript(t *testing.T) {
	if _, err := datamigrate.NewTransformVM("", "starlark"); err == nil {
		t.Error("expected error for empty script")
	}
}

// TestNewTransformVM_RejectsUnknownLang — v2.3 only ships
// starlark.
func TestNewTransformVM_RejectsUnknownLang(t *testing.T) {
	_, err := datamigrate.NewTransformVM(`def transform(v): return v`, "tengo")
	if err == nil {
		t.Fatal("expected error for non-starlark lang")
	}
	if !strings.Contains(err.Error(), "starlark") {
		t.Errorf("expected starlark-only error, got: %v", err)
	}
}

// TestNewTransformVM_RejectsCompileError — invalid Starlark
// surfaces at NewTransformVM, not at first Apply.
func TestNewTransformVM_RejectsCompileError(t *testing.T) {
	_, err := datamigrate.NewTransformVM(`def transform(value): retur value`, "starlark") // missing 'n'
	if err == nil {
		t.Fatal("expected compile error")
	}
}

// TestTransformVM_RejectsNonBytesReturn — script returns a
// dict / int / string instead of bytes; runtime error.
func TestTransformVM_RejectsNonBytesReturn(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    return {"not": "bytes"}
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	_, _, err = vm.Apply([]byte("x"))
	if err == nil {
		t.Fatal("expected error for non-bytes return")
	}
	if !strings.Contains(err.Error(), "want bytes or None") {
		t.Errorf("expected wrong-type error, got: %v", err)
	}
}

// TestTransformVM_StepLimit — runaway loop hits the
// transformMaxSteps cap, surfaces as a runtime error rather
// than blocking the host process.
func TestTransformVM_StepLimit(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    n = 0
    for _ in range(10000000):
        n = n + 1
    return value
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	_, _, err = vm.Apply([]byte("x"))
	if err == nil {
		t.Fatal("expected step-limit error")
	}
}

// TestTransformVM_NoFileSystemAccess — Starlark is a true
// sandbox; `open` / `os` / `subprocess` aren't predeclared.
// Script trying to use them errors as "undefined".
func TestTransformVM_NoFileSystemAccess(t *testing.T) {
	for _, expr := range []string{
		`open("/etc/passwd")`,
		`os.system("rm -rf /")`,
		`subprocess.run(["ls"])`,
	} {
		_, err := datamigrate.NewTransformVM("def transform(value):\n    "+expr+"\n    return value\n", "starlark")
		if err == nil {
			t.Errorf("expected sandbox refusal for: %s", expr)
		}
	}
}

// TestTransformVM_ConcurrentSafe — the same compiled VM
// drives multiple goroutines without state leakage. Each
// Apply gets its own thread, so parallel workers (the
// dialect Applier's pool) can share a single TransformVM.
func TestTransformVM_ConcurrentSafe(t *testing.T) {
	vm, err := datamigrate.NewTransformVM(`
def transform(value):
    return bytes(str(value).replace("hello", "HELLO"))
`, "starlark")
	if err != nil {
		t.Fatalf("NewTransformVM: %v", err)
	}
	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			out, changed, err := vm.Apply([]byte("hello world"))
			if err != nil {
				errCh <- err
				return
			}
			if !changed {
				errCh <- nil
				return
			}
			if !strings.Contains(string(out), "HELLO world") {
				errCh <- nil
			}
			errCh <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent Apply: %v", err)
		}
	}
}
