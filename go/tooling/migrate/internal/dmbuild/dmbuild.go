// Package dmbuild holds the data-migration build helpers shared by the
// non-transactional KV/object backends (internal/redis, internal/s3). Each
// such backend has to turn a *datamigrate.Migration body into the runtime
// objects it executes — the protobuf codec and the per-op Starlark VMs — and
// the build logic is backend-agnostic (it touches only datamigrate types).
// Keeping it here means a fix lands once instead of once per backend.
package dmbuild

import (
	"fmt"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// Codec returns the protobuf codec for migrations whose `encoding: protobuf`
// field is set, nil for `encoding: json`. Errors here are migration-refusal —
// re-running with the same body would fail identically.
func Codec(mig *datamigrate.Migration) (*datamigrate.ProtoCodec, error) {
	if mig.Encoding != datamigrate.EncodingProtobuf {
		return nil, nil
	}
	fdsBytes, err := datamigrate.DecodeProtoDescriptor(mig.ProtoDescriptor)
	if err != nil {
		return nil, err
	}
	return datamigrate.NewProtoCodec(fdsBytes, mig.ProtoMessage)
}

// TransformVMs compiles every TRANSFORM_FIELD op's script up front so a
// malformed script aborts the migration before any store mutation runs (and so
// compilation cost doesn't repeat per-key inside the worker pool). Returns nil
// when no TRANSFORM_FIELD ops are present, otherwise a map keyed by op index.
func TransformVMs(mig *datamigrate.Migration) (map[int]*datamigrate.TransformVM, error) {
	var vms map[int]*datamigrate.TransformVM
	for i, op := range mig.Operations {
		if op.Op != datamigrate.OpTransformField {
			continue
		}
		vm, err := datamigrate.NewTransformVM(op.Script, op.ScriptLang)
		if err != nil {
			return nil, fmt.Errorf("op[%d] TRANSFORM_FIELD: %w", i, err)
		}
		if vms == nil {
			vms = map[int]*datamigrate.TransformVM{}
		}
		vms[i] = vm
	}
	return vms, nil
}
