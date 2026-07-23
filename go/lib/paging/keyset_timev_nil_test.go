package paging_test

import (
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// TestScalarOf_TimeV_NilInner pins the nil-guard in ScalarOf's TimeV
// arm: a KeysetValue whose oneof IS set to the TimeV variant but whose
// inner *timestamppb.Timestamp is nil decodes to the Go zero time
// (time.Time{}, year 1), NOT to the Unix epoch.
//
// This distinction matters: protobuf's Timestamp.AsTime() is nil-safe
// and would return time.Unix(0,0).UTC() (1970-01-01) for a nil
// receiver. The explicit guard instead yields IsZero()==true, so a
// boundary value carrying an absent timestamp sorts as "the beginning
// of time" (Go zero) consistently with how other zero-valued keyset
// columns are treated — and is distinguishable from a real epoch
// timestamp that a row might legitimately hold.
//
// The variant is set (so this is NOT the empty-oneof panic path that
// TestScalarOf_UnsetPanics covers); only the inner message is nil.
func TestScalarOf_TimeV_NilInner(t *testing.T) {
	kv := &w17pb.KeysetValue{Value: &w17pb.KeysetValue_TimeV{TimeV: nil}}

	got, ok := paging.ScalarOf(kv).(time.Time)
	if !ok {
		t.Fatalf("ScalarOf returned %T, want time.Time", paging.ScalarOf(kv))
	}
	if !got.IsZero() {
		t.Errorf("ScalarOf(nil TimeV) = %v, want the Go zero time (IsZero)", got)
	}
	// Specifically NOT the Unix epoch that a nil-safe AsTime() would give.
	if got.Equal(time.Unix(0, 0).UTC()) {
		t.Errorf("ScalarOf(nil TimeV) decoded to the Unix epoch; the nil guard should yield Go-zero instead")
	}
}
