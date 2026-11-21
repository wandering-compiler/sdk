package dqlbind

import (
	"bytes"
	"math"
	"testing"
)

type testEnum int32
type testName string

func TestScanArray_TextForms(t *testing.T) {
	var s []string
	if err := ScanArray(&s).Scan(`{a,"b,c","d\"e","f\\g",NULL2,""}`); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b,c", `d"e`, `f\g`, "NULL2", ""}
	if len(s) != len(want) {
		t.Fatalf("got %q", s)
	}
	for i := range want {
		if s[i] != want[i] {
			t.Fatalf("element %d = %q, want %q", i, s[i], want[i])
		}
	}

	var n []int64
	if err := ScanArray(&n).Scan([]byte("{1,-2,3}")); err != nil || len(n) != 3 || n[1] != -2 {
		t.Fatalf("int64 %v %v", n, err)
	}
	var u []uint32
	if err := ScanArray(&u).Scan("{4294967295}"); err != nil || u[0] != math.MaxUint32 {
		t.Fatalf("uint32 %v %v", u, err)
	}
	var b []bool
	if err := ScanArray(&b).Scan("{t,f}"); err != nil || !b[0] || b[1] {
		t.Fatalf("bool %v %v", b, err)
	}
	var f []float64
	if err := ScanArray(&f).Scan("{1.5,NaN,-Infinity}"); err != nil || f[0] != 1.5 || !math.IsNaN(f[1]) || !math.IsInf(f[2], -1) {
		t.Fatalf("float64 %v %v", f, err)
	}
	var by [][]byte
	if err := ScanArray(&by).Scan(`{"\\x0102","\\x"}`); err != nil || !bytes.Equal(by[0], []byte{1, 2}) || len(by[1]) != 0 {
		t.Fatalf("bytea %v %v", by, err)
	}
	var e []testEnum
	if err := ScanArray(&e).Scan("{1,2}"); err != nil || e[1] != 2 {
		t.Fatalf("enum %v %v", e, err)
	}
	var nm []testName
	if err := ScanArray(&nm).Scan("{x}"); err != nil || nm[0] != "x" {
		t.Fatalf("named string %v %v", nm, err)
	}
}

func TestScanArray_EmptyNullAndRefusals(t *testing.T) {
	s := []string{"stale"}
	if err := ScanArray(&s).Scan("{}"); err != nil || s == nil || len(s) != 0 {
		t.Fatalf("empty array: %v %v", s, err)
	}
	if err := ScanArray(&s).Scan(nil); err != nil || s != nil {
		t.Fatalf("NULL array leaves the slice nil: %v %v", s, err)
	}
	if err := ScanArray(&s).Scan("[0:1]={a,b}"); err != nil || len(s) != 2 {
		t.Fatalf("dimension decoration: %v %v", s, err)
	}
	for _, bad := range []any{"{a,NULL}", "{{1},{2}}", "not an array", `{"open}`, 42} {
		if err := ScanArray(&s).Scan(bad); err == nil {
			t.Errorf("%v scanned without an error", bad)
		}
	}
	var n []int32
	if err := ScanArray(&n).Scan("{2147483648}"); err == nil {
		t.Error("an out-of-range int32 element scanned")
	}
}
