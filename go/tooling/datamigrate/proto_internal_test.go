package datamigrate

import "testing"

// TestUnmarshal_EmptyBody pins the empty-body refusal: the apply tool
// must treat a zero-length body as a refusal-to-apply, not a silent
// empty migration. (Distinct from malformed-YAML, which fails later in
// the decoder.)
func TestUnmarshal_EmptyBody(t *testing.T) {
	if _, err := Unmarshal(nil); err == nil {
		t.Error("nil body must be refused")
	}
	if _, err := Unmarshal([]byte{}); err == nil {
		t.Error("empty body must be refused")
	}
}

// TestToInt64_EveryWidth pins toInt64's documented contract — it
// normalises every integer width yaml.v3 (or a programmatic caller)
// might produce to int64. yaml.v3 itself only emits int/int64, so the
// narrower + unsigned arms are unreachable through Unmarshal and are
// exercised directly. The uint64 overflow arm is the one rejection.
func TestToInt64_EveryWidth(t *testing.T) {
	ok := []struct {
		name string
		in   any
		want int64
	}{
		{"int", int(7), 7},
		{"int8", int8(-8), -8},
		{"int16", int16(16), 16},
		{"int32", int32(-32), -32},
		{"int64", int64(64), 64},
		{"uint", uint(1), 1},
		{"uint8", uint8(8), 8},
		{"uint16", uint16(16), 16},
		{"uint32", uint32(32), 32},
		{"uint64 in range", uint64(99), 99},
		{"nil", nil, 0},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			got, err := toInt64(c.in)
			if err != nil || got != c.want {
				t.Errorf("toInt64(%v) = (%d, %v), want (%d, nil)", c.in, got, err, c.want)
			}
		})
	}
	if _, err := toInt64(uint64(1<<63) + 1); err == nil {
		t.Error("uint64 above MaxInt64 must overflow-error")
	}
	if _, err := toInt64("nope"); err == nil {
		t.Error("non-integer must error")
	}
}

// TestToUint64_EveryWidth pins toUint64's contract: int/int64/uint/
// uint64/nil normalise to uint64, and the signed arms reject negatives
// explicitly rather than wrapping to a huge unsigned value.
func TestToUint64_EveryWidth(t *testing.T) {
	ok := []struct {
		name string
		in   any
		want uint64
	}{
		{"int", int(3), 3},
		{"int64 non-negative", int64(64), 64},
		{"uint", uint(5), 5},
		{"uint64", uint64(99), 99},
		{"nil", nil, 0},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			got, err := toUint64(c.in)
			if err != nil || got != c.want {
				t.Errorf("toUint64(%v) = (%d, %v), want (%d, nil)", c.in, got, err, c.want)
			}
		})
	}
	if _, err := toUint64(int(-1)); err == nil {
		t.Error("negative int must error")
	}
	if _, err := toUint64(int64(-1)); err == nil {
		t.Error("negative int64 must error")
	}
	if _, err := toUint64("nope"); err == nil {
		t.Error("non-integer must error")
	}
}
