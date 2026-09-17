package migrate

import (
	"context"
	"fmt"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
)

// StoreSchemaState is what a store can say about whether it already holds a
// schema. Three values, not a bool, because "I cannot tell" and "there is
// nothing here" lead to opposite decisions and a bool collapses them: a KV
// store has no fingerprint and is genuinely re-appliable, while an EMPTY
// relational store is a fact a caller may need to refuse on.
type StoreSchemaState int

const (
	// StoreSchemaUnknown — the store cannot answer (no fingerprint
	// capability). Callers must proceed as they would have without asking.
	StoreSchemaUnknown StoreSchemaState = iota
	// StoreSchemaEmpty — the store answered, and it holds no schema.
	StoreSchemaEmpty
	// StoreSchemaPopulated — the store answered, and it holds one.
	StoreSchemaPopulated
)

// emptyFingerprint is what FingerprintCapable reports for a store holding no
// schema — the fingerprint of the empty Schema, not the empty string, which is
// what a store that failed to answer would give.
var emptyFingerprint = fingerprint.Schema{}.FingerprintHex()

// StoreSchemaStateOf asks one connection's store whether it already holds a
// schema.
//
// Single reader for the question: `migrate schema` asks it to decide whether
// to leave an existing database alone, and the dev diff-apply asks it to
// decide whether a checkpoint describes the database it is pointed at. Two
// readers of a fingerprint would be two chances to disagree about what an
// empty string means.
func StoreSchemaStateOf(ctx context.Context, applierFor ApplierFor, conn string) (StoreSchemaState, error) {
	ap, err := applierFor(conn)
	if err != nil {
		return StoreSchemaUnknown, err
	}
	defer func() { _ = ap.Close() }()
	fp, ok := ap.(FingerprintCapable)
	if !ok {
		return StoreSchemaUnknown, nil
	}
	got, err := fp.Fingerprint(ctx)
	if err != nil {
		return StoreSchemaUnknown, fmt.Errorf("read the store's schema state: %w", err)
	}
	if got == "" {
		return StoreSchemaUnknown, nil
	}
	if got == emptyFingerprint {
		return StoreSchemaEmpty, nil
	}
	return StoreSchemaPopulated, nil
}

// StoreSchemaEmptyFingerprint is the fingerprint a store holding no schema
// reports. Exported for tests that need to stand up an empty store without
// recomputing it — a second computation of "what empty looks like" is exactly
// the duplicate that lets the two drift apart.
func StoreSchemaEmptyFingerprint() string { return emptyFingerprint }
