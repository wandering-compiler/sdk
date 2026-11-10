package i18n

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
)

// FormatTableJSON is the embedded formats.json byte for byte (what a
// generator bakes into an artifact), and a fresh copy each call.
func TestFormatTableJSON_IsTheEmbeddedFileByteForByte(t *testing.T) {
	want, err := os.ReadFile("formats.json")
	if err != nil {
		t.Fatal(err)
	}
	got := FormatTableJSON()
	if !bytes.Equal(got, want) {
		t.Fatal("FormatTableJSON differs from formats.json")
	}
	got[0] ^= 0xff
	if !bytes.Equal(FormatTableJSON(), want) {
		t.Fatal("mutating the returned slice changed the embedded table")
	}
}

// DecimalString never renders exponent notation, at either end of the
// float range: the decimal parser rejects it.
func TestDecimalString_NeverRendersAnExponent(t *testing.T) {
	for _, v := range []float64{1e21, 1e-7, 123456789012345678901234567890.0, 5e-324, math.MaxFloat64, -1e-10, 0.1 + 0.2} {
		s := DecimalString(v)
		if strings.ContainsAny(s, "eE") {
			t.Errorf("DecimalString(%g) = %q carries an exponent", v, s)
		}
		if _, ok := parseDecimal(s); !ok {
			t.Errorf("DecimalString(%g) = %q does not parse as a decimal", v, s)
		}
	}
}
