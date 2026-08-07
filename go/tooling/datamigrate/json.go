package datamigrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// JSONApplyOp transforms a single JSON-encoded value by
// applying the field-level Operation (ADD_FIELD_DEFAULT,
// REMOVE_FIELD, RENAME_FIELD). Used by encoding=json data
// migrations on every KV dialect (apply/redis,
// apply/s3 — Phase E v1).
//
// Inputs:
//
//   - `raw` is the existing value bytes from the KV store
//     (Redis GET / S3 GetObject). Empty bytes (zero-length
//     value) short-circuit to (nil, false, nil) — there's
//     nothing to mutate.
//   - `op` is the parsed Operation. Caller is responsible for
//     filtering by Keyspace before calling — JSONApplyOp
//     trusts the op already applies to this key.
//
// Returns:
//
//   - `newRaw` is the re-encoded JSON bytes when `changed` is
//     true. Nil when `changed` is false.
//   - `changed` reports whether the value was actually mutated.
//     False = idempotent no-op (already-correct value); caller
//     skips the SET / Put network write.
//   - `err` covers JSON decode / encode failure + unsupported
//     op kinds.
//
// Replay safety per kind:
//
//   - ADD_FIELD_DEFAULT only sets when the field is missing.
//   - REMOVE_FIELD only deletes when the field is present.
//   - RENAME_FIELD only acts when From is present.
//
// All three are idempotent — re-running on already-mutated
// values yields (nil, false, nil), enabling resume-after-
// interrupt without per-key cursor tracking.
func JSONApplyOp(raw []byte, op Operation) ([]byte, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}

	// UseNumber so numeric fields decode to json.Number (verbatim
	// digits) rather than float64. Without it, an untouched field
	// holding an int64 > 2^53 (e.g. a snowflake ID) or a value like
	// `1.10` / `1e6` would be silently re-encoded with lost precision
	// or changed representation — data corruption in fields the
	// migration never references.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("decode JSON: %w", err)
	}

	changed := false
	switch op.Op {
	case OpAddFieldDefault:
		if _, present := doc[op.Field]; !present {
			doc[op.Field] = op.Value
			changed = true
		}
	case OpRemoveField:
		if _, present := doc[op.Field]; present {
			delete(doc, op.Field)
			changed = true
		}
	case OpRenameField:
		if v, present := doc[op.From]; present {
			// Clobber guard: if both From and To are present the
			// rename would overwrite a distinct destination value and
			// lose it. Abort so the migration surfaces the conflict
			// rather than silently corrupting data. A destination that
			// already holds the SAME value is treated as benign (drop
			// the source, stay idempotent).
			if existing, collision := doc[op.To]; collision && !reflect.DeepEqual(existing, v) {
				return nil, false, fmt.Errorf(
					"RENAME_FIELD %q→%q: destination field already present with a different value — refusing to overwrite", op.From, op.To)
			}
			doc[op.To] = v
			delete(doc, op.From)
			changed = true
		}
	default:
		return nil, false, fmt.Errorf("unsupported op %q", op.Op)
	}

	if !changed {
		return nil, false, nil
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Errorf("encode JSON: %w", err)
	}
	return out, true, nil
}
