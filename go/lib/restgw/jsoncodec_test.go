package restgw

import (
	"encoding/base64"
	"math"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestAppendJSONString_Escaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"tab\tnl\n", `"tab\tnl\n"`},
		{"<>&", `"<>&"`}, // NOT HTML-escaped (protojson parity)
		// remaining short escapes
		{"cr\r", "\"cr\\r\""},
		{"bs\b", "\"bs\\b\""},
		{"ff\f", "\"ff\\f\""},
		// other control chars < 0x20 take the default \u00XX arm
		{"\x01", "\"\\u0001\""},
		{"\x1f", "\"\\u001f\""},
		// a control char mid-string flushes the preceding run first
		{"ab\x00cd", "\"ab\\u0000cd\""},
	}
	for _, c := range cases {
		if got := string(AppendJSONString(nil, c.in)); got != c.want {
			t.Errorf("AppendJSONString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestAppendJSONNumbers(t *testing.T) {
	if got := string(AppendJSONInt64(nil, -42)); got != `"-42"` {
		t.Errorf("int64 = %s, want quoted", got)
	}
	if got := string(AppendJSONUint64(nil, 42)); got != `"42"` {
		t.Errorf("uint64 = %s, want quoted", got)
	}
	if got := string(AppendJSONInt32(nil, -7)); got != `-7` {
		t.Errorf("int32 = %s, want bare", got)
	}
	if got := string(AppendJSONFloat64(nil, 1.5)); got != `1.5` {
		t.Errorf("float = %s", got)
	}
	if got := string(AppendJSONFloat64(nil, math.NaN())); got != `"NaN"` {
		t.Errorf("NaN = %s", got)
	}
	if got := string(AppendJSONFloat64(nil, math.Inf(1))); got != `"Infinity"` {
		t.Errorf("+Inf = %s", got)
	}
	if got := string(AppendJSONFloat64(nil, math.Inf(-1))); got != `"-Infinity"` {
		t.Errorf("-Inf = %s", got)
	}
	if got := string(AppendJSONBool(nil, true)); got != `true` {
		t.Errorf("bool true = %s", got)
	}
	if got := string(AppendJSONBool(nil, false)); got != `false` {
		t.Errorf("bool false = %s", got)
	}
}

func TestAppendJSONBytes_Base64(t *testing.T) {
	if got := string(AppendJSONBytes(nil, []byte("hi!"))); got != `"aGkh"` {
		t.Errorf("bytes = %s, want base64 \"aGkh\"", got)
	}
	if got := string(AppendJSONBytes(nil, nil)); got != `""` {
		t.Errorf("empty bytes = %s", got)
	}
	// Grow-loop arm: an input whose base64 length far exceeds the small-
	// slice capacity forces the in-place grow loop to run before encoding.
	big := make([]byte, 48) // base64 → 64 chars, well past initial cap
	want := `"` + base64.StdEncoding.EncodeToString(big) + `"`
	if got := string(AppendJSONBytes(nil, big)); got != want {
		t.Errorf("grow-loop bytes = %q, want %q", got, want)
	}
}

// emailMarshaler is a stand-in for a GENERATED marshaler: it appends the
// w17-dialect JSON for the test Email message using the leaf encoders +
// concrete field reads (here via reflection, since the fixture is
// dynamic). MarshalProto must prefer it over protojson once registered.
func emailMarshaler(dst []byte, m proto.Message) ([]byte, error) {
	r := m.ProtoReflect()
	addr := r.Get(r.Descriptor().Fields().ByName("address")).String()
	dst = append(dst, '{')
	dst = append(dst, `"address":`...)
	dst = AppendJSONString(dst, addr)
	return append(dst, '}'), nil
}

func TestMarshalProto_PrefersRegisteredMarshaler(t *testing.T) {
	desc := oneofTestDesc(t).Fields().ByName("email").Message()
	name := desc.FullName()
	RegisterJSONMarshaler(name, emailMarshaler)
	defer delete(jsonMarshalers, name)

	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))
	got, err := MarshalProto(m)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	if string(got) != `{"address":"a@b.c"}` {
		t.Errorf("registered marshaler not used / wrong output: %s", got)
	}
	// Sanity: unregistering falls back to protojson (which is equivalent
	// here).
	delete(jsonMarshalers, name)
	got2, _ := MarshalProto(m)
	if string(got2) != `{"address":"a@b.c"}` {
		t.Errorf("fallback output = %s", got2)
	}
}

// Perf thesis: a generated marshaler appending into a reused buffer is
// allocation-free in steady state, vs protojson's reflective marshal.
func BenchmarkMarshal_GeneratedAppend(b *testing.B) {
	desc := buildEmailDesc(b)
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _ := emailMarshaler(buf[:0], m)
		_ = out
	}
}

func BenchmarkMarshal_Protojson(b *testing.B) {
	desc := buildEmailDesc(b)
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("address"), protoreflect.ValueOfString("a@b.c"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _ := Marshaller.Marshal(m)
		_ = out
	}
}

// buildEmailDesc returns the test Email message descriptor (no oneof) for
// the benchmarks — shares oneofTestDesc's file so no extra fixture.
func buildEmailDesc(tb testing.TB) protoreflect.MessageDescriptor {
	return oneofTestDesc(tb).Fields().ByName("email").Message()
}
