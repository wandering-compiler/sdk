package datamigrate

import "fmt"

// Validate enforces v1 schema invariants on a Migration:
//
//   - version > 0 and ≤ CurrentVersion (refuses bodies emitted
//     by a newer compiler we don't know how to read)
//   - encoding is "json" (v1) — "protobuf" rejected with
//     "deferred to v2"
//   - operations[] non-empty (an empty data migration is
//     a producer bug — empty bodies aren't shipped)
//   - per-operation field invariants (see validateOp)
//   - parallel ≥ 0 (zero falls back to DefaultParallel)
//
// Returns the first error encountered. Callers — Marshal
// (compile-time) + Unmarshal (apply-time) — both invoke
// Validate so identical rules apply both directions.
func Validate(m *Migration) error {
	if m == nil {
		return fmt.Errorf("nil migration")
	}
	if m.Version <= 0 {
		return fmt.Errorf("version must be > 0; got %d", m.Version)
	}
	if m.Version > CurrentVersion {
		return fmt.Errorf("version %d unknown to this build (max %d) — upgrade w17migrate", m.Version, CurrentVersion)
	}
	switch m.Encoding {
	case EncodingJSON:
		if m.ProtoDescriptor != "" || m.ProtoMessage != "" {
			return fmt.Errorf("encoding=json must not carry proto_descriptor / proto_message")
		}
	case EncodingProtobuf:
		// v2.2 (D-iter3-19) graduated protobuf encoding from
		// the deferred list. Both proto_descriptor (base64
		// FileDescriptorSet) + proto_message (FQN) are
		// required so the apply-side codec resolves to a
		// concrete dynamicpb.MessageType.
		if m.ProtoDescriptor == "" {
			return fmt.Errorf("encoding=protobuf requires proto_descriptor (base64 FileDescriptorSet)")
		}
		if m.ProtoMessage == "" {
			return fmt.Errorf("encoding=protobuf requires proto_message (fully-qualified message name)")
		}
	case "":
		return fmt.Errorf("encoding is required (v1: json, protobuf)")
	default:
		return fmt.Errorf("encoding %q unknown (supported: json, protobuf)", m.Encoding)
	}
	if m.Parallel < 0 {
		return fmt.Errorf("parallel must be ≥ 0; got %d", m.Parallel)
	}
	if len(m.Operations) == 0 {
		return fmt.Errorf("operations[] is empty — empty data migrations should not be emitted")
	}
	for i, op := range m.Operations {
		if err := validateOp(op); err != nil {
			return fmt.Errorf("operations[%d]: %w", i, err)
		}
	}
	return nil
}

// validateOp checks per-kind required fields.
func validateOp(op Operation) error {
	if op.Keyspace == "" {
		return fmt.Errorf("keyspace is required")
	}
	// script / script_lang only belong on TRANSFORM_FIELD ops;
	// any other op kind carrying them is malformed.
	if op.Op != OpTransformField && (op.Script != "" || op.ScriptLang != "") {
		return fmt.Errorf("script / script_lang reserved for TRANSFORM_FIELD; %s carries them by mistake", op.Op)
	}
	switch op.Op {
	case OpAddFieldDefault:
		if op.Field == "" {
			return fmt.Errorf("ADD_FIELD_DEFAULT: field is required")
		}
		if op.From != "" || op.To != "" {
			return fmt.Errorf("ADD_FIELD_DEFAULT: from/to fields belong on RENAME_FIELD, not here")
		}
		// op.Value may be any (including nil) — nil = explicit
		// JSON null default, valid.
	case OpRemoveField:
		if op.Field == "" {
			return fmt.Errorf("REMOVE_FIELD: field is required")
		}
		if op.From != "" || op.To != "" || op.Value != nil {
			return fmt.Errorf("REMOVE_FIELD: from/to/value fields don't belong here")
		}
	case OpRenameField:
		if op.From == "" || op.To == "" {
			return fmt.Errorf("RENAME_FIELD: from + to are required")
		}
		if op.From == op.To {
			return fmt.Errorf("RENAME_FIELD: from == to is a no-op; reject")
		}
		if op.Field != "" || op.Value != nil {
			return fmt.Errorf("RENAME_FIELD: use from/to, not field/value")
		}
	case OpTransformField:
		// Graduated to v2.3 (D-iter3-20). script + script_lang
		// are required; field / from / to / value don't apply
		// (the script is the whole transform).
		if op.Script == "" {
			return fmt.Errorf("TRANSFORM_FIELD: script is required")
		}
		if op.ScriptLang == "" {
			return fmt.Errorf("TRANSFORM_FIELD: script_lang is required (v2.3: starlark)")
		}
		if op.ScriptLang != scriptLangStarlark {
			return fmt.Errorf("TRANSFORM_FIELD: script_lang %q not supported (v2.3: starlark)", op.ScriptLang)
		}
		if op.Field != "" || op.From != "" || op.To != "" || op.Value != nil {
			return fmt.Errorf("TRANSFORM_FIELD: field / from / to / value don't apply — the script is the transform")
		}
		return nil
	case "":
		return fmt.Errorf("op is required")
	default:
		return fmt.Errorf("op %q unknown (v2.3 supports: ADD_FIELD_DEFAULT, REMOVE_FIELD, RENAME_FIELD, TRANSFORM_FIELD)", op.Op)
	}
	return nil
}

// EffectiveParallel returns the worker count to use given the
// migration's project-side default + the operator's optional
// CLI override. Override > 0 wins; otherwise migration's
// `parallel` field; otherwise DefaultParallel.
//
// Callers — apply/redis, apply/s3 — invoke this once per
// migration to determine their goroutine pool size.
func EffectiveParallel(m *Migration, cliOverride int) int {
	if cliOverride > 0 {
		return cliOverride
	}
	if m != nil && m.Parallel > 0 {
		return m.Parallel
	}
	return DefaultParallel
}

// InverseOperations auto-derives the down-direction
// operations[] for a forward-direction Migration. Mapping:
//
//	ADD_FIELD_DEFAULT (forward) ↔ REMOVE_FIELD (down)
//	RENAME_FIELD (forward)      ↔ RENAME_FIELD (swap From/To)
//	REMOVE_FIELD (forward)      → IRREVERSIBLE
//	TRANSFORM_FIELD (forward)   → IRREVERSIBLE (script transform
//	                              is generally not reversible
//	                              without an explicit
//	                              author-supplied inverse)
//
// Returns the inverse migration + a slice of operation indices
// that were marked irreversible. Caller (compiler-side emit)
// uses this to:
//   - emit the down body when no irreversible ops present
//   - emit a `# wc:irreversible: ...` marker + skip the down
//     body when REMOVE_FIELD or TRANSFORM_FIELD is in the
//     forward set
func InverseOperations(m *Migration) (*Migration, []int) {
	if m == nil {
		return nil, nil
	}
	out := &Migration{
		Version:          m.Version,
		Encoding:         m.Encoding,
		ProtoDescriptor:  m.ProtoDescriptor,
		ProtoMessage:     m.ProtoMessage,
		Parallel:         m.Parallel,
		EstimatedRecords: m.EstimatedRecords,
		Operations:       make([]Operation, 0, len(m.Operations)),
	}
	var irreversible []int
	// Reverse op order — last applied undoes first.
	for i := len(m.Operations) - 1; i >= 0; i-- {
		op := m.Operations[i]
		switch op.Op {
		case OpAddFieldDefault:
			out.Operations = append(out.Operations, Operation{
				Op:       OpRemoveField,
				Keyspace: op.Keyspace,
				Field:    op.Field,
			})
		case OpRenameField:
			out.Operations = append(out.Operations, Operation{
				Op:       OpRenameField,
				Keyspace: op.Keyspace,
				From:     op.To,
				To:       op.From,
			})
		case OpRemoveField:
			irreversible = append(irreversible, i)
		case OpTransformField:
			irreversible = append(irreversible, i)
		}
	}
	return out, irreversible
}
