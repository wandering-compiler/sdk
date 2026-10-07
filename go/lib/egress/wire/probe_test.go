package wire

import (
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Findings of the phase 3 review, one test each, on the hand-written
// probe.proto.

func probeMsg(t *testing.T, name, protoJSON string) *dynamicpb.Message {
	t.Helper()
	m := dynamicpb.NewMessage(mdIn(t, probe, name))
	if protoJSON != "" {
		if err := protojson.Unmarshal([]byte(protoJSON), m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func decodeProbe(t *testing.T, name, js string) (*dynamicpb.Message, error) {
	t.Helper()
	m := probeMsg(t, name, "")
	return m, Unmarshal([]byte(js), m)
}

// A google.protobuf.Value holds null, so a null in its array or map is a
// value — refusing it failed a whole valid response.
func TestNullInsideFreeFormCollections(t *testing.T) {
	m, err := decodeProbe(t, "Loose", `{"items":[1,null,"x"],"meta":{"k":null,"j":2}}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"items":[1,null,"x"],"meta":{"j":2,"k":null}}` {
		t.Errorf("got %s", out)
	}
	if _, err := decodeProbe(t, "Loose", `{"list":[1,null]}`); err == nil || !strings.Contains(err.Error(), "/list/1: null in an array") {
		t.Errorf("a null in an array of int64 is still refused: %v", err)
	}
}

// Arms that differ only by an optional property: the one declaring fewer
// properties is the exact fit for an object that has none of the extras.
func TestUntaggedArmsDifferingByAnOptionalProperty(t *testing.T) {
	for js, want := range map[string]string{
		`{"ab":{"r":1,"label":"x"}}`:             "a",
		`{"ab":{"r":1,"label":"x","extra":"y"}}`: "b",
		`{"ab":{"r":1}}`:                         "a",
	} {
		m, err := decodeProbe(t, "Holder", js)
		if err != nil {
			t.Errorf("%s: %v", js, err)
			continue
		}
		if got := armOf(m, "ab"); got != want {
			t.Errorf("%s: arm %q, want %q", js, got, want)
		}
	}
}

// An arm that cannot carry the discriminator is refused, not sent bare.
func TestDiscriminatedNonObjectArm(t *testing.T) {
	m := probeMsg(t, "Holder", `{"bad": {"free": {"x": 1}}}`)
	if _, err := Marshal(m); err == nil || !strings.Contains(err.Error(), `arm free of a union discriminated by "kind" is not an object`) {
		t.Errorf("Marshal: %v", err)
	}
	if _, err := Form(m); err == nil || !strings.Contains(err.Error(), "is not an object") {
		t.Errorf("Form: %v", err)
	}
	ok := probeMsg(t, "Holder", `{"bad": {"a": {"r": 2}}}`)
	if out, err := Marshal(ok); err != nil || string(out) != `{"bad":{"kind":"a","r":2}}` {
		t.Errorf("an object arm: %s %v", out, err)
	}
}

// Empty (and Any) arms: no panic, an object takes them.
func TestEmptyArm(t *testing.T) {
	for js, want := range map[string]string{`{"e":{}}`: "empty", `{"e":"x"}`: "s"} {
		m, err := decodeProbe(t, "Holder", js)
		if err != nil {
			t.Errorf("%s: %v", js, err)
			continue
		}
		if got := armOf(m, "e"); got != want {
			t.Errorf("%s: arm %q, want %q", js, got, want)
		}
	}
}

// Numbers are read exactly, by the JSON grammar, or refused.
func TestNumbers(t *testing.T) {
	get := func(m *dynamicpb.Message, f string) protoreflect.Value {
		return m.Get(mdIn(t, probe, "Loose").Fields().ByName(protoreflect.Name(f)))
	}
	m, err := decodeProbe(t, "Loose", `{"i":1234567890123456789.0,"u":1.0,"d":"2.5e3","w":"7"}`)
	if err != nil {
		t.Fatal(err)
	}
	if get(m, "i").Int() != 1234567890123456789 || get(m, "u").Uint() != 1 || get(m, "d").Float() != 2500 {
		t.Errorf("i=%d u=%d d=%v", get(m, "i").Int(), get(m, "u").Uint(), get(m, "d").Float())
	}
	for _, js := range []string{`{"i":"1_000"}`, `{"i":"+5"}`, `{"d":"NaN"}`, `{"d":"-inf"}`, `{"i":1.5}`, `{"u":-1}`, `{"i":9223372036854775808}`} {
		if _, err := decodeProbe(t, "Loose", js); err == nil {
			t.Errorf("%s was accepted", js)
		}
	}
	// A wrapper's 64-bit integer is a number too, not protojson's string.
	out, err := Marshal(probeMsg(t, "Loose", `{"w": "9007199254740993"}`))
	if err != nil || string(out) != `{"w":9007199254740993}` {
		t.Errorf("Int64Value: %s %v", out, err)
	}
}

func TestForm_KeysAndScalars(t *testing.T) {
	if _, err := Form(probeMsg(t, "Loose", `{"m": {"a]b[c": "v"}}`)); err == nil || !strings.Contains(err.Error(), "cannot be written unambiguously") {
		t.Errorf("a key with brackets: %v", err)
	}
	if _, err := Form(probeMsg(t, "Scalar", `{"value": 1.5}`)); err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Errorf("a scalar body: %v", err)
	}
	vals, err := Form(probeMsg(t, "Loose", `{"d": 1e300, "ts": "2026-10-07T12:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("d") != "1e+300" || vals.Get("ts") != "2026-10-07T12:00:00Z" {
		t.Errorf("d=%q ts=%q", vals.Get("d"), vals.Get("ts"))
	}
}

func TestMarshal_InvalidUTF8(t *testing.T) {
	m := probeMsg(t, "Loose", "")
	m.Set(mdIn(t, probe, "Loose").Fields().ByName("s"), protoreflect.ValueOfString("a\xffb"))
	if _, err := Marshal(m); err == nil || !strings.Contains(err.Error(), "/s: string is not valid UTF-8") {
		t.Errorf("err = %v", err)
	}
}

// Unmarshal replaces: a reused message carries nothing over.
func TestUnmarshal_Replaces(t *testing.T) {
	m, err := decodeProbe(t, "Loose", `{"list":[1],"s":"old"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal([]byte(`{"list":[2]}`), m); err != nil {
		t.Fatal(err)
	}
	out, _ := Marshal(m)
	if string(out) != `{"list":[2]}` {
		t.Errorf("got %s", out)
	}
}

// Round two of the review: a refused key must refuse the object under it
// too (it used to move the object to the top level), an empty key is
// refused, and a Value's NaN has no form.
func TestForm_RefusedKeysRefuseObjects(t *testing.T) {
	for name, js := range map[string]string{
		"struct key with a bracket": `{"st": {"a]": {"x": 1}}}`,
		"empty map key":             `{"m": {"": "v"}}`,
		"empty struct key":          `{"st": {"": 1}}`,
	} {
		if vals, err := Form(probeMsg(t, "Loose", js)); err == nil {
			t.Errorf("%s: accepted as %s", name, vals.Encode())
		}
	}
	m := probeMsg(t, "Loose", "")
	st := m.Mutable(mdIn(t, probe, "Loose").Fields().ByName("st")).Message()
	fields := st.Mutable(st.Descriptor().Fields().ByName("fields")).Map()
	v := fields.NewValue()
	v.Message().Set(v.Message().Descriptor().Fields().ByName("number_value"), protoreflect.ValueOfFloat64(math.NaN()))
	fields.Set(protoreflect.ValueOfString("n").MapKey(), v)
	if _, err := Form(m); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Errorf("a Value's NaN: %v", err)
	}
}

// A huge exponent is settled from the text, before any big arithmetic.
func TestNumbers_HugeExponentsAreCheap(t *testing.T) {
	start := time.Now()
	for _, js := range []string{`{"i":1e100000000}`, `{"i":1e899999999}`, `{"i":"1` + strings.Repeat("0", 400) + `"}`, `{"i":1.` + strings.Repeat("0", 320) + `1}`} {
		if _, err := decodeProbe(t, "Loose", js); err == nil {
			t.Errorf("%.40s… was accepted", js)
		}
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("refusing took %s", d)
	}
	for js, want := range map[string]int64{`{"i":0e1000000}`: 0, `{"i":12e-1}`: 0, `{"i":1200e-2}`: 12, `{"i":-9.223372036854775808e18}`: -9223372036854775808} {
		m, err := decodeProbe(t, "Loose", js)
		if want == 0 && js == `{"i":12e-1}` {
			if err == nil {
				t.Errorf("%s (1.2) was accepted as an integer", js)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", js, err)
			continue
		}
		if got := m.Get(mdIn(t, probe, "Loose").Fields().ByName("i")).Int(); got != want {
			t.Errorf("%s = %d, want %d", js, got, want)
		}
	}
}

// Round three: a tiny non-zero number is not an integer (it used to
// underflow to 0 and pass), and any zero is zero.
func TestNumbers_TinyAndZero(t *testing.T) {
	for _, js := range []string{`{"i":1e-999999999}`, `{"i":5e-2000000000}`, `{"i":1e-99999999999999999999}`, `{"u":0.5}`} {
		if _, err := decodeProbe(t, "Loose", js); err == nil || !strings.Contains(err.Error(), "expected an integer") {
			t.Errorf("%s: %v", js, err)
		}
	}
	for _, js := range []string{`{"i":0e99999999999999999999}`, `{"i":-0e5}`, `{"u":0.0e-7}`} {
		if _, err := decodeProbe(t, "Loose", js); err != nil {
			t.Errorf("%s: %v", js, err)
		}
	}
}
