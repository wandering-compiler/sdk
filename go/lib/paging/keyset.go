package paging

import (
	"fmt"
	"time"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ScalarOf returns the underlying Go scalar of a KeysetValue
// (int64 / string / []byte / bool / float64 / time.Time).
// Used by storage-codegen-emitted code when binding boundary
// values into SQL parameters.
//
// Panics on an empty oneof — codegen guarantees every
// KeysetValue produced by EncodeCursor has its oneof set, so
// a missing variant indicates a corrupted cursor that survived
// schema_version validation, which is a server bug.
func ScalarOf(kv *w17pb.KeysetValue) any {
	switch v := kv.GetValue().(type) {
	case *w17pb.KeysetValue_IntV:
		return v.IntV
	case *w17pb.KeysetValue_UintV:
		return v.UintV
	case *w17pb.KeysetValue_StringV:
		return v.StringV
	case *w17pb.KeysetValue_BytesV:
		return v.BytesV
	case *w17pb.KeysetValue_BoolV:
		return v.BoolV
	case *w17pb.KeysetValue_DoubleV:
		return v.DoubleV
	case *w17pb.KeysetValue_TimeV:
		if v.TimeV == nil {
			return time.Time{}
		}
		return v.TimeV.AsTime()
	default:
		panic(fmt.Sprintf("paging: KeysetValue oneof unset (value=%T)", v))
	}
}

// FromInt64 wraps an int64 as a KeysetValue. Helper for
// gateway-codegen-emitted cursor-build code.
func FromInt64(v int64) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_IntV{IntV: v}}
}

// FromUint64 wraps a uint64 as a KeysetValue. Used for a
// proto uint64 sort key so a value above int64 max is not
// wrapped negative (OF-3). Mirrors FromInt64.
func FromUint64(v uint64) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_UintV{UintV: v}}
}

// FromString wraps a string as a KeysetValue.
func FromString(v string) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_StringV{StringV: v}}
}

// FromBytes wraps a []byte as a KeysetValue.
func FromBytes(v []byte) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_BytesV{BytesV: v}}
}

// FromBool wraps a bool as a KeysetValue.
func FromBool(v bool) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_BoolV{BoolV: v}}
}

// FromFloat64 wraps a float64 as a KeysetValue.
func FromFloat64(v float64) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_DoubleV{DoubleV: v}}
}

// FromTime wraps a time.Time as a KeysetValue.
func FromTime(v time.Time) *w17pb.KeysetValue {
	return &w17pb.KeysetValue{Value: &w17pb.KeysetValue_TimeV{TimeV: timestamppb.New(v)}}
}

// ClampLimit applies a default + cap to a caller-supplied
// page-size value. Used by gateway handlers when parsing the
// `?limit=` query parameter:
//
//   - requested == 0 → default
//   - requested > max → max
//   - otherwise → requested
//
// Both default and max come from the endpoint's PagedConfig.
func ClampLimit(requested, def, max uint32) uint32 {
	if requested == 0 {
		requested = def
	}
	if max > 0 && requested > max {
		requested = max
	}
	return requested
}
