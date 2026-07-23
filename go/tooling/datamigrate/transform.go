// Package datamigrate — TRANSFORM_FIELD escape hatch via
// embedded Starlark (Phase E v2.3 — D-iter3-20).
//
// **Why Starlark.** Pure-Go embedded interpreter (used by Bazel /
// Buildkite). The sandbox properties come from the language
// design itself, not a containment layer:
//
//   - No `import` of arbitrary modules; only what we
//     pre-declare on the predeclared dict.
//   - No file system / network / subprocess builtins.
//   - No reflection on Go state.
//   - Step limit (`thread.SetMaxExecutionSteps`) bounds runaway
//     loops — the host process never blocks indefinitely on a
//     malicious or buggy script.
//   - Compiled to immutable bytecode; the same `*starlark.Program`
//     can be safely shared by parallel workers (each gets its
//     own `*starlark.Thread`).
//
// **No memory cap.** The sandbox bounds CPU (the step limit) but
// NOT memory — go.starlark.net has no allocation ceiling, so a
// script can still OOM the host by building a huge value. We do
// not add one (an accurate Starlark memory limiter is intrusive
// and fragile). The trust model therefore does NOT treat the
// script body as adversarial for memory: migration bodies are
// authored + SIGNED (the lock is signed for codegen
// reproducibility), so the operator vouches for the script they
// run — the step limit is a runaway-loop kill-switch for buggy
// scripts, not a defence against a hostile one.
//
// **Contract.** The migration body's `script:` field MUST define
// a `transform(value)` function. Apply tool calls it once per
// keyspace match:
//
//   - Input `value` is a Starlark `bytes` holding the raw
//     KV-store bytes (whatever encoding — JSON, protobuf, custom).
//   - Return `bytes` for "replace value", or `None` for "no-op /
//     skip the SET / Put". Any other return type is an error.
//
// **Encoding-agnostic** by design. The script handles parse +
// modify + encode itself — for JSON the predeclared `json`
// module provides `json.encode` / `json.decode`; for protobuf
// the operator works at byte / hex level (until v2.4 brings
// dynamicpb-aware helpers).
package datamigrate

import (
	"bytes"
	"fmt"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// transformMaxSteps caps Starlark execution per key. 1M steps
// is generous — typical transform ~100 steps; this is the
// runaway-loop kill-switch.
//
// NOTE: this bounds CPU (step count) only — there is NO memory
// cap (go.starlark.net has no allocation ceiling), so a script
// can still OOM the host by building a huge value. That is an
// accepted limit: migration bodies are signed/authored, not
// adversarial — see the package doc's "No memory cap" note.
const transformMaxSteps = 1_000_000

// TransformVM is a compiled Starlark migration script. Built
// once per TRANSFORM_FIELD op (compilation cost paid up front;
// shared across parallel workers). Apply per key creates a
// fresh `*starlark.Thread` — Threads carry per-call state
// (call stack, step counter) and aren't safe for concurrent
// reuse.
type TransformVM struct {
	fn starlark.Value // the `transform` callable resolved at compile time
}

// NewTransformVM compiles the script + extracts the
// `transform` callable. Errors here are migration-refusal
// (the same body would fail again on retry).
//
//   - Parse + execute the script's top level once. Top-level
//     statements run with no input arguments — typically just
//     `def transform(value): ...` declarations.
//   - Look up `transform` in the resulting globals. Missing or
//     non-callable yields an explicit error.
//
// `lang` is the script_lang field on the Operation; v2.3 only
// accepts "starlark". Unknown values error so future
// graduations (lua / tengo) are obvious.
func NewTransformVM(script, lang string) (*TransformVM, error) {
	if script == "" {
		return nil, fmt.Errorf("script is empty")
	}
	if lang != scriptLangStarlark {
		return nil, fmt.Errorf("script_lang %q not supported (v2.3: starlark)", lang)
	}
	thread := &starlark.Thread{Name: "transform.compile"}
	// Bound the COMPILE thread too: ExecFileOptions runs the script's
	// top level, and FileOptions enables While/TopLevelControl, so a
	// script with a top-level `while True:` would otherwise hang the host
	// (deploy/migration) process indefinitely — the per-key exec limit
	// alone doesn't cover compile time (filegen-sec-2).
	thread.SetMaxExecutionSteps(transformMaxSteps)
	predeclared := starlark.StringDict{
		"json": json.Module,
	}
	opts := &syntax.FileOptions{
		Set:               true,
		While:             true,
		TopLevelControl:   true,
		GlobalReassign:    true,
		LoadBindsGlobally: false,
	}
	globals, err := starlark.ExecFileOptions(opts, thread, "transform.star", script, predeclared)
	if err != nil {
		return nil, fmt.Errorf("compile script: %w", err)
	}
	fn, ok := globals["transform"]
	if !ok {
		return nil, fmt.Errorf("script must define a top-level `transform(value)` function")
	}
	if _, ok := fn.(starlark.Callable); !ok {
		return nil, fmt.Errorf("`transform` is not callable (got %s)", fn.Type())
	}
	return &TransformVM{fn: fn}, nil
}

// Apply runs the compiled `transform` function against one
// raw value. Mirrors JSONApplyOp / ProtoCodec.ApplyOp:
//
//   - Empty bytes short-circuit to (nil, false, nil) — no
//     transform on a nothing-value, consistent with the other
//     codecs.
//   - Returns (newRaw, true, nil) when the script produced a
//     bytes value different from the input.
//   - Returns (nil, false, nil) when the script returned None
//     OR returned bytes byte-identical to the input (treated
//     as an explicit no-op).
//   - Returns (nil, false, err) for compile / runtime / type
//     errors — caller propagates and the migration aborts.
func (vm *TransformVM) Apply(raw []byte) ([]byte, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	thread := &starlark.Thread{Name: "transform.exec"}
	thread.SetMaxExecutionSteps(transformMaxSteps)

	valIn := starlark.Bytes(raw)
	result, err := starlark.Call(thread, vm.fn, starlark.Tuple{valIn}, nil)
	if err != nil {
		return nil, false, fmt.Errorf("script execution: %w", err)
	}
	if result == starlark.None {
		return nil, false, nil
	}
	out, ok := result.(starlark.Bytes)
	if !ok {
		return nil, false, fmt.Errorf("transform returned %s; want bytes or None", result.Type())
	}
	outBytes := []byte(out)
	if bytes.Equal(outBytes, raw) {
		return nil, false, nil
	}
	return outBytes, true, nil
}

// scriptLangStarlark is the only supported value for
// Operation.ScriptLang in v2.3. Future graduations land as
// new constants alongside.
const scriptLangStarlark = "starlark"
