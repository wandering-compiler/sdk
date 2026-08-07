package restgw

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAppendJSONUint32AndFloat32(t *testing.T) {
	if got := string(AppendJSONUint32(nil, 4294967295)); got != "4294967295" {
		t.Errorf("uint32 = %q", got)
	}
	for _, c := range []struct {
		v    float32
		want string
	}{{1.5, "1.5"}, {float32(math.NaN()), `"NaN"`}, {float32(math.Inf(1)), `"Infinity"`}, {float32(math.Inf(-1)), `"-Infinity"`}} {
		if got := string(AppendJSONFloat32(nil, c.v)); got != c.want {
			t.Errorf("float32(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestAppendTimestampJSON(t *testing.T) {
	if got := string(AppendTimestampJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q", got)
	}
	base := time.Date(2009, 2, 13, 23, 31, 30, 0, time.UTC)
	cases := []struct {
		ns   int
		want string
	}{
		{0, `"2009-02-13T23:31:30Z"`},                   // no fraction
		{500000000, `"2009-02-13T23:31:30.500Z"`},       // trimmed to 3 digits
		{123456789, `"2009-02-13T23:31:30.123456789Z"`}, // full 9 digits
	}
	for _, c := range cases {
		ts := timestamppb.New(time.Date(2009, 2, 13, 23, 31, 30, c.ns, time.UTC))
		if got := string(AppendTimestampJSON(nil, ts)); got != c.want {
			t.Errorf("ts ns=%d = %q, want %q", c.ns, got, c.want)
		}
	}
	_ = base
}

func TestAppendDurationJSON(t *testing.T) {
	if got := string(AppendDurationJSON(nil, nil)); got != "null" {
		t.Errorf("nil = %q", got)
	}
	cases := []struct {
		d    *durationpb.Duration
		want string
	}{
		{&durationpb.Duration{Seconds: 5}, `"5s"`},
		{&durationpb.Duration{Seconds: -3}, `"-3s"`},
		{&durationpb.Duration{Seconds: 1, Nanos: 500000000}, `"1.500s"`},
		{&durationpb.Duration{Seconds: -1, Nanos: -500000000}, `"-1.500s"`},
	}
	for _, c := range cases {
		if got := string(AppendDurationJSON(nil, c.d)); got != c.want {
			t.Errorf("duration %v = %q, want %q", c.d, got, c.want)
		}
	}
}
