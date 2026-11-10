package restgw

import (
	"bytes"
	"math"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The float leaves the generated marshalers emit MUST be byte-identical to
// protojson's: 'f' form unless |v| < 1e-6 or >= 1e21, then 'e' with the
// negative exponent's zero padding stripped.
func TestFastPathFloatLeaves_AreByteIdenticalToProtojson(t *testing.T) {
	for _, v := range []float64{0, -0.5, 1, 1.5, 1000000, 123456789, 1e20, 1e21, 1.5e300, 1e-6, 1e-7, 0.0000001234, -2.5e-10, math.MaxFloat64, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), math.Inf(-1)} {
		ref, err := protojson.Marshal(wrapperspb.Double(v))
		if err != nil {
			t.Fatal(err)
		}
		if fast := AppendJSONFloat64(nil, v); !bytes.Equal(fast, ref) {
			t.Errorf("double %v: fast %s, protojson %s", v, fast, ref)
		}
		f := float32(v)
		ref, err = protojson.Marshal(wrapperspb.Float(f))
		if err != nil {
			t.Fatal(err)
		}
		if fast := AppendJSONFloat32(nil, f); !bytes.Equal(fast, ref) {
			t.Errorf("float %v: fast %s, protojson %s", f, fast, ref)
		}
	}
}

// E2.6 holds on the generated fast path too: a string field carrying
// invalid UTF-8 is a marshal error (protojson's), never bytes written
// verbatim into a body that is then not valid JSON.
func TestMarshalProto_InvalidUTF8ErrorsOnTheFastPath(t *testing.T) {
	name := (&wrapperspb.StringValue{}).ProtoReflect().Descriptor().FullName()
	RegisterJSONMarshaler(name, func(dst []byte, m proto.Message) ([]byte, error) {
		dst = append(dst, `{"value":`...)
		dst = AppendJSONString(dst, m.(*wrapperspb.StringValue).GetValue())
		return append(dst, '}'), nil
	})
	t.Cleanup(func() { delete(jsonMarshalers, name) })

	if out, err := MarshalProto(wrapperspb.String("ok")); err != nil || string(out) != `{"value":"ok"}` {
		t.Fatalf("valid string: %s, %v (the fast path must still serve it)", out, err)
	}
	if out, err := MarshalProto(wrapperspb.String("\xff\xfe")); err == nil {
		t.Fatalf("invalid UTF-8 marshalled to %q without an error", out)
	}
	dst := []byte("prefix")
	if out, err := MarshalProtoAppend(dst, wrapperspb.String("a\xffb")); err == nil || string(out) != "prefix" {
		t.Fatalf("append path: %q, %v; want the error and dst untouched", out, err)
	}
}
