package paging_test

import (
	"math"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/proto"
)

// OF-3: a uint64 sort key above int64 max must survive the
// cursor round-trip unwrapped, and ScalarOf must hand back the
// true unsigned value so the keyset SQL comparison binds the
// unsigned boundary (not a wrapped-negative int64).
func TestFromUint64_RoundTrip_AboveInt64Max(t *testing.T) {
	// A value strictly greater than math.MaxInt64; int64(v)
	// would wrap this negative.
	const big uint64 = math.MaxInt64 + 12345

	kv := paging.FromUint64(big)

	// ScalarOf returns the concrete unsigned value (bound into
	// SQL as-is) rather than a narrowed / wrapped int64.
	got, ok := paging.ScalarOf(kv).(uint64)
	if !ok {
		t.Fatalf("ScalarOf: want uint64, got %T", paging.ScalarOf(kv))
	}
	if got != big {
		t.Fatalf("ScalarOf: want %d, got %d", big, got)
	}

	// Encode → decode preserves the unsigned value bit-for-bit.
	req := paging.FromString("filter=open")
	token, err := paging.EncodeCursor(
		req, []*w17pb.KeysetValue{kv}, 0,
		w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey,
	)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotReq := &w17pb.KeysetValue{}
	gotB, _, _, _, err := paging.DecodeCursor(token, gotReq, testSchemaVersion, testKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(gotB) != 1 {
		t.Fatalf("boundaries: want 1, got %d", len(gotB))
	}
	if !proto.Equal(kv, gotB[0]) {
		t.Fatalf("boundary mismatch:\n  want: %v\n  got:  %v", kv, gotB[0])
	}
	if rt := gotB[0].GetUintV(); rt != big {
		t.Fatalf("decoded uint_v: want %d, got %d", big, rt)
	}
	// Guard against the old narrowing: the int_v arm must be
	// unset so nothing reads a wrapped int64.
	if _, isInt := gotB[0].GetValue().(*w17pb.KeysetValue_IntV); isInt {
		t.Fatal("decoded boundary used int_v arm — uint64 was narrowed to int64 (OF-3 regression)")
	}
}
