package certgen

import "testing"

// zeroReader yields an unbounded stream of zero bytes — it drives
// rand.Int to its minimum return value (0), the one draw RFC 5280
// §4.1.2.2 forbids for a certificate serial.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// Q44-certgen-1: rand.Int returns a value in [0, 2^128), so a serial of
// 0 is possible — RFC 5280 §4.1.2.2 requires a POSITIVE integer, and a
// zero serial can be rejected by strict validators. randSerialFrom must
// never return a non-positive serial. Driven with an all-zero source the
// raw rand.Int draw is 0; the +1 shift makes the serial 1.
func TestRandSerialFrom_NeverZero(t *testing.T) {
	s, err := randSerialFrom(zeroReader{})
	if err != nil {
		t.Fatalf("randSerialFrom: %v", err)
	}
	if s.Sign() <= 0 {
		t.Fatalf("serial = %s, want a positive integer (RFC 5280 §4.1.2.2)", s)
	}
}
