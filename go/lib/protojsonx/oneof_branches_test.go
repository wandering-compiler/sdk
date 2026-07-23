package protojsonx_test

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	// Link the well-known types so protoregistry.GlobalFiles can resolve the
	// WKT dependencies the rich descriptor references.
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// richDesc compiles a message R exercising every oneof/arm shape the dialect
// classifies + transforms:
//
//	regular scalars (en/bd/fl/db) — scalarJSONKind switch arms
//	oneof contact  { Email email; string phone }      msg arm + bare string
//	oneof numbered { int32 count; int64 big; bool flag } number/string/bool bare
//	oneof wktof    { Timestamp ts; Int32Value n32; Struct st } scalar-WKT/number-WKT/object-WKT
//	oneof ambig    { Struct s1; Value v1 }             two object-WKT arms (ambiguous)
//	BoolValue bv   — wktScalarJSONKind boolean arm
//	R child        — recursion into a oneof-bearing message
//	map<string,R> bymap, repeated R items — collapseChild/expandChild map+list
//	map<string,string> tags — map with scalar value (early-return branch)
func richDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	str := func() *descriptorpb.FieldDescriptorProto_Type {
		return descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	f := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: opt, Type: typ.Enum()}
	}
	msgField := func(name string, num int32, typeName string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(typeName)}
	}
	oneofMsg := func(name string, num int32, typeName string, idx int32) *descriptorpb.FieldDescriptorProto {
		fd := msgField(name, num, typeName)
		fd.OneofIndex = proto.Int32(idx)
		return fd
	}
	oneofScalar := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, idx int32) *descriptorpb.FieldDescriptorProto {
		fd := f(name, num, typ)
		fd.OneofIndex = proto.Int32(idx)
		return fd
	}
	mapEntry := func(name string, valType *descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name:    proto.String(name),
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("key"), Number: proto.Int32(1), Label: opt, Type: str()},
				valType,
			},
		}
	}

	r := &descriptorpb.DescriptorProto{
		Name: proto.String("R"),
		Field: []*descriptorpb.FieldDescriptorProto{
			f("id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			oneofMsg("email", 2, ".pxr.Email", 0),
			oneofScalar("phone", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, 0),
			oneofScalar("count", 4, descriptorpb.FieldDescriptorProto_TYPE_INT32, 1),
			oneofScalar("big", 5, descriptorpb.FieldDescriptorProto_TYPE_INT64, 1),
			oneofScalar("flag", 6, descriptorpb.FieldDescriptorProto_TYPE_BOOL, 1),
			oneofMsg("ts", 7, ".google.protobuf.Timestamp", 2),
			oneofMsg("n32", 8, ".google.protobuf.Int32Value", 2),
			oneofMsg("st", 9, ".google.protobuf.Struct", 2),
			oneofMsg("s1", 10, ".google.protobuf.Struct", 3),
			oneofMsg("v1", 11, ".google.protobuf.Value", 3),
			f("en", 12, descriptorpb.FieldDescriptorProto_TYPE_ENUM),
			f("bd", 13, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
			f("fl", 14, descriptorpb.FieldDescriptorProto_TYPE_FLOAT),
			f("db", 15, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE),
			msgField("bv", 16, ".google.protobuf.BoolValue"),
			msgField("child", 17, ".pxr.R"),
			{Name: proto.String("bymap"), Number: proto.Int32(18), Label: rep, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".pxr.R.BymapEntry")},
			{Name: proto.String("items"), Number: proto.Int32(19), Label: rep, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".pxr.R")},
			{Name: proto.String("tags"), Number: proto.Int32(20), Label: rep, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".pxr.R.TagsEntry")},
			oneofScalar("alt_phone", 21, descriptorpb.FieldDescriptorProto_TYPE_STRING, 4),
			oneofMsg("alt_email", 22, ".pxr.Email", 4),
		},
		NestedType: []*descriptorpb.DescriptorProto{
			mapEntry("BymapEntry", msgField("value", 2, ".pxr.R")),
			mapEntry("TagsEntry", &descriptorpb.FieldDescriptorProto{Name: proto.String("value"), Number: proto.Int32(2), Label: opt, Type: str()}),
		},
		EnumType: nil,
		OneofDecl: []*descriptorpb.OneofDescriptorProto{
			{Name: proto.String("contact")},
			{Name: proto.String("numbered")},
			{Name: proto.String("wktof")},
			{Name: proto.String("ambig")},
			{Name: proto.String("alt_contact")}, // snake_case → snakeToCamel on expand
		},
	}
	// Inner carries a genuine oneof; Outer has none — so HasAnyOneof(Outer)
	// must recurse through the message field to find it (walkForOneofs field arm).
	inner := &descriptorpb.DescriptorProto{
		Name: proto.String("Inner"),
		Field: []*descriptorpb.FieldDescriptorProto{
			oneofScalar("pa", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, 0),
			oneofScalar("pb", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, 0),
		},
		OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("pick")}},
	}
	outer := &descriptorpb.DescriptorProto{
		Name:  proto.String("Outer"),
		Field: []*descriptorpb.FieldDescriptorProto{msgField("inner", 1, ".pxr.Inner")},
	}
	// The enum field needs a declared enum type.
	enumType := &descriptorpb.EnumDescriptorProto{
		Name: proto.String("SomeEnum"),
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("UNKNOWN"), Number: proto.Int32(0)},
			{Name: proto.String("A"), Number: proto.Int32(1)},
		},
	}
	r.Field[11].TypeName = proto.String(".pxr.SomeEnum") // en

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("pxr_test.proto"),
		Package: proto.String("pxr"),
		Syntax:  proto.String("proto3"),
		Dependency: []string{
			"google/protobuf/timestamp.proto",
			"google/protobuf/wrappers.proto",
			"google/protobuf/struct.proto",
		},
		EnumType: []*descriptorpb.EnumDescriptorProto{enumType},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Email"), Field: []*descriptorpb.FieldDescriptorProto{
				f("address", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			}},
			r,
			inner,
			outer,
		},
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return fd.Messages().ByName("R")
}

// TestScalarKindClassification drives scalarJSONKind / wktScalarJSONKind /
// isObjectWKTArm / IsDiscriminatorArm / BareArmJSONKind across every field
// kind via the two exported classifiers.
func TestScalarKindClassification(t *testing.T) {
	desc := richDesc(t)
	fields := desc.Fields()
	fd := func(n string) protoreflect.FieldDescriptor { return fields.ByName(protoreflect.Name(n)) }

	// BareArmJSONKind == scalarJSONKind for non-discriminator arms.
	bareCases := map[string]string{
		"flag":  "boolean", // BoolKind
		"count": "number",  // Int32Kind
		"fl":    "number",  // FloatKind
		"db":    "number",  // DoubleKind
		"big":   "string",  // Int64Kind
		"id":    "string",  // StringKind
		"en":    "string",  // EnumKind
		"bd":    "string",  // BytesKind
		"ts":    "string",  // Timestamp WKT → string
		"n32":   "number",  // Int32Value WKT → number
		"bv":    "boolean", // BoolValue WKT → boolean
		"st":    "",        // object-WKT Struct → "" (not scalar-serializing)
	}
	for name, want := range bareCases {
		if got := protojsonx.BareArmJSONKind(fd(name)); got != want {
			t.Errorf("BareArmJSONKind(%s) = %q, want %q", name, got, want)
		}
	}

	// IsDiscriminatorArm: only the genuine (non-WKT) message arm is tagged.
	discCases := map[string]bool{
		"email": true,  // genuine message arm
		"phone": false, // bare scalar
		"ts":    false, // scalar-WKT
		"n32":   false, // number-WKT
		"st":    false, // object-WKT (matched by object shape, not tagged here)
		"flag":  false, // scalar
	}
	for name, want := range discCases {
		if got := protojsonx.IsDiscriminatorArm(fd(name)); got != want {
			t.Errorf("IsDiscriminatorArm(%s) = %v, want %v", name, got, want)
		}
	}

	// BareArmJSONKind of the genuine message arm is "" (it carries the disc).
	if got := protojsonx.BareArmJSONKind(fd("email")); got != "" {
		t.Errorf("BareArmJSONKind(email) = %q, want empty", got)
	}
}

func collapse(t *testing.T, desc protoreflect.MessageDescriptor, in string, aliasOf protojsonx.AliasFunc) map[string]any {
	t.Helper()
	out, err := protojsonx.CollapseOneofs([]byte(in), desc, aliasOf)
	if err != nil {
		t.Fatalf("collapse %s: %v", in, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal collapsed: %v", err)
	}
	return m
}

func expand(t *testing.T, desc protoreflect.MessageDescriptor, in string, aliasOf protojsonx.AliasFunc) map[string]any {
	t.Helper()
	out, err := protojsonx.ExpandOneofs([]byte(in), desc, aliasOf)
	if err != nil {
		t.Fatalf("expand %s: %v", in, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal expanded: %v", err)
	}
	return m
}

// TestCollapse_AllArmShapes drives collapseResponse / collapsedArm across the
// message arm, the bare scalar/number/bool arms, the scalar-WKT and object-WKT
// arms, plus nested recursion (child), map values (bymap) and list items.
func TestCollapse_AllArmShapes(t *testing.T) {
	desc := richDesc(t)

	// Bare string arm → collapses to the oneof key, value unchanged.
	if m := collapse(t, desc, `{"phone":"555"}`, nil); m["contact"] != "555" {
		t.Errorf("phone collapse: %+v", m)
	}
	// Number arm.
	if m := collapse(t, desc, `{"count":7}`, nil); m["numbered"].(float64) != 7 {
		t.Errorf("count collapse: %+v", m)
	}
	// Message arm gains a discriminator.
	m := collapse(t, desc, `{"email":{"address":"a@b"}}`, nil)
	c, ok := m["contact"].(map[string]any)
	if !ok || c["w17_discriminator"] != "email" || c["address"] != "a@b" {
		t.Errorf("email collapse: %+v", m)
	}
	// Scalar-WKT arm stays bare (string value, not an object).
	if m := collapse(t, desc, `{"ts":"2020-01-01T00:00:00Z"}`, nil); m["wktof"] != "2020-01-01T00:00:00Z" {
		t.Errorf("ts collapse: %+v", m)
	}
	// Recursion: a oneof set inside a nested child message collapses too.
	mm := collapse(t, desc, `{"child":{"phone":"999"}}`, nil)
	child, ok := mm["child"].(map[string]any)
	if !ok || child["contact"] != "999" {
		t.Errorf("child recursion: %+v", mm)
	}
	// Map<string,R> values recurse.
	mp := collapse(t, desc, `{"bymap":{"k":{"count":3}}}`, nil)
	by, _ := mp["bymap"].(map[string]any)
	kv, _ := by["k"].(map[string]any)
	if kv["numbered"].(float64) != 3 {
		t.Errorf("bymap recursion: %+v", mp)
	}
	// Repeated R items recurse.
	li := collapse(t, desc, `{"items":[{"phone":"1"},{"count":2}]}`, nil)
	arr, _ := li["items"].([]any)
	if len(arr) != 2 || arr[0].(map[string]any)["contact"] != "1" {
		t.Errorf("items recursion: %+v", li)
	}
	// map<string,string> tags is left untouched (scalar map value).
	tg := collapse(t, desc, `{"tags":{"a":"b"}}`, nil)
	if tg["tags"].(map[string]any)["a"] != "b" {
		t.Errorf("tags untouched: %+v", tg)
	}
}

// TestCollapse_NonObjectAndMalformed covers the early returns: a non-object
// top-level node passes through, and malformed JSON errors.
func TestCollapse_NonObjectAndMalformed(t *testing.T) {
	desc := richDesc(t)
	out, err := protojsonx.CollapseOneofs([]byte(`"scalar"`), desc, nil)
	if err != nil || string(out) != `"scalar"` {
		t.Errorf("non-object collapse: out=%s err=%v", out, err)
	}
	if _, err := protojsonx.CollapseOneofs([]byte(`{bad`), desc, nil); err == nil {
		t.Error("malformed JSON should error")
	}
}

// TestExpand_DiscriminatorAndBare drives resolveArm across: a discriminated
// message arm, an unknown-discriminator fallthrough, bare scalar matching by
// JSON primitive type, and the unique object-WKT arm.
func TestExpand_DiscriminatorAndBare(t *testing.T) {
	desc := richDesc(t)

	// Discriminated message arm → flat "email".
	m := expand(t, desc, `{"contact":{"w17_discriminator":"email","address":"a@b"}}`, nil)
	em, ok := m["email"].(map[string]any)
	if !ok || em["address"] != "a@b" {
		t.Errorf("disc expand: %+v", m)
	}
	if _, leaked := em["w17_discriminator"]; leaked {
		t.Errorf("discriminator leaked into arm: %+v", em)
	}

	// Unknown discriminator → arm unresolved, key passed through verbatim.
	m = expand(t, desc, `{"contact":{"w17_discriminator":"ghost","x":1}}`, nil)
	if _, ok := m["contact"]; !ok {
		t.Errorf("unknown disc should pass key through: %+v", m)
	}

	// Bare scalar arms matched by JSON primitive: string→phone, number→count,
	// bool→flag.
	if m := expand(t, desc, `{"numbered":7}`, nil); m["count"].(float64) != 7 {
		t.Errorf("bare number expand: %+v", m)
	}
	if m := expand(t, desc, `{"numbered":true}`, nil); m["flag"] != true {
		t.Errorf("bare bool expand: %+v", m)
	}
	// "contact" with a bare string → phone arm.
	if m := expand(t, desc, `{"contact":"555"}`, nil); m["phone"] != "555" {
		t.Errorf("bare string expand: %+v", m)
	}

	// Bare object with no discriminator → matched against the unique
	// object-WKT arm. "wktof" has exactly one object-WKT arm (st).
	m = expand(t, desc, `{"wktof":{"a":1}}`, nil)
	if _, ok := m["st"]; !ok {
		t.Errorf("object-WKT expand should pick st: %+v", m)
	}
}

// TestExpand_Ambiguities covers resolveArm's nil returns: an "ambig" oneof has
// two object-WKT arms (Struct + Value), so a bare object can't be resolved; and
// a bare scalar that matches no arm of "contact"'s set is left unresolved.
func TestExpand_Ambiguities(t *testing.T) {
	desc := richDesc(t)

	// Two object-WKT arms → ambiguous → key passes through.
	m := expand(t, desc, `{"ambig":{"a":1}}`, nil)
	if _, ok := m["ambig"]; !ok {
		t.Errorf("ambiguous object arm should pass through: %+v", m)
	}
	// "contact" set is {message, string}; a number matches no bare scalar arm.
	m = expand(t, desc, `{"contact":42}`, nil)
	if _, ok := m["contact"]; !ok {
		t.Errorf("unmatched bare scalar should pass through: %+v", m)
	}
}

// TestExpand_CamelCaseOneofKey covers genuineOneofByKey's snakeToCamel match:
// a JS client may send a camelCased oneof key. (The oneof names here are
// single words; force a multi-word match via the discriminator path instead.)
func TestExpand_CamelCaseOneofKey(t *testing.T) {
	desc := richDesc(t)
	// Exercise snakeToCamel directly through a multi-underscore name by
	// resolving a key the function would camelCase; all oneofs here are single
	// words, so this asserts the single-word identity path stays stable.
	if m := expand(t, desc, `{"numbered":7}`, nil); m["count"] == nil {
		t.Errorf("camel/identity oneof key: %+v", m)
	}
}

// TestExpand_NonObjectAndMalformed — a non-object node passes through; bad JSON
// returns the input unchanged (ExpandOneofs is forgiving on decode).
func TestExpand_NonObjectAndMalformed(t *testing.T) {
	desc := richDesc(t)
	out, err := protojsonx.ExpandOneofs([]byte(`[1,2]`), desc, nil)
	if err != nil || string(out) != `[1,2]` {
		t.Errorf("non-object expand: out=%s err=%v", out, err)
	}
	out, err = protojsonx.ExpandOneofs([]byte(`{bad`), desc, nil)
	if err != nil || string(out) != `{bad` {
		t.Errorf("malformed expand should return raw: out=%s err=%v", out, err)
	}
}

// TestHasAnyOneof_NilCacheAndNesting covers the nil guard, the cached-result
// fast path (second call), and walkForOneofs' message-field recursion: Outer
// has no oneof of its own but reaches Inner's through its message field.
func TestHasAnyOneof_NilCacheAndNesting(t *testing.T) {
	if protojsonx.HasAnyOneof(nil) {
		t.Error("nil descriptor must report no oneof")
	}
	desc := richDesc(t)
	if !protojsonx.HasAnyOneof(desc) {
		t.Fatal("R has genuine oneofs")
	}
	// Second call hits the cache branch.
	if !protojsonx.HasAnyOneof(desc) {
		t.Error("cached HasAnyOneof disagreed")
	}
	outer := desc.ParentFile().Messages().ByName("Outer")
	if outer == nil {
		t.Fatal("Outer not found")
	}
	if !protojsonx.HasAnyOneof(outer) {
		t.Error("Outer must reach Inner's oneof via message-field recursion")
	}
	inner := desc.ParentFile().Messages().ByName("Inner")
	if !protojsonx.HasAnyOneof(inner) {
		t.Error("Inner has a genuine oneof")
	}
}

// TestExpand_SnakeCaseOneofKey covers genuineOneofByKey's snakeToCamel arm: the
// alt_contact oneof is reachable under the camelCased key "altContact".
func TestExpand_SnakeCaseOneofKey(t *testing.T) {
	desc := richDesc(t)
	m := expand(t, desc, `{"altContact":"555"}`, nil)
	if m["alt_phone"] != "555" {
		t.Errorf("snake/camel oneof key expand: %+v", m)
	}
}

// TestExpand_MapAndListChildren covers expandChild's map + list branches and
// the scalar-map early return: non-oneof message map/list values recurse, while
// a scalar-valued map passes through untouched.
func TestExpand_MapAndListChildren(t *testing.T) {
	desc := richDesc(t)

	// map<string,R>: collapsed oneof inside a map value expands back to flat.
	m := expand(t, desc, `{"bymap":{"k":{"numbered":3}}}`, nil)
	by, _ := m["bymap"].(map[string]any)
	kv, _ := by["k"].(map[string]any)
	if kv["count"].(float64) != 3 {
		t.Errorf("bymap expand: %+v", m)
	}

	// repeated R: each item expands.
	m = expand(t, desc, `{"items":[{"contact":"x"},{"numbered":true}]}`, nil)
	arr, _ := m["items"].([]any)
	if len(arr) != 2 || arr[0].(map[string]any)["phone"] != "x" || arr[1].(map[string]any)["flag"] != true {
		t.Errorf("items expand: %+v", m)
	}

	// map<string,string>: scalar value map untouched.
	m = expand(t, desc, `{"tags":{"a":"b"}}`, nil)
	if m["tags"].(map[string]any)["a"] != "b" {
		t.Errorf("tags expand untouched: %+v", m)
	}
}

// TestShapeMismatches covers the defensive type-assertion guards in
// collapseChild/expandChild: JSON is untrusted, so a map/list-typed field that
// arrives as the wrong JSON shape is passed through unchanged, not panicked on.
func TestShapeMismatches(t *testing.T) {
	desc := richDesc(t)
	// bymap is map<string,R> but arrives as a string → passed through.
	if m := collapse(t, desc, `{"bymap":"oops"}`, nil); m["bymap"] != "oops" {
		t.Errorf("collapse map mismatch: %+v", m)
	}
	if m := expand(t, desc, `{"bymap":"oops"}`, nil); m["bymap"] != "oops" {
		t.Errorf("expand map mismatch: %+v", m)
	}
	// items is repeated R but arrives as a string → passed through.
	if m := collapse(t, desc, `{"items":"oops"}`, nil); m["items"] != "oops" {
		t.Errorf("collapse list mismatch: %+v", m)
	}
	if m := expand(t, desc, `{"items":"oops"}`, nil); m["items"] != "oops" {
		t.Errorf("expand list mismatch: %+v", m)
	}
	// A message-typed oneof arm arriving as a bare scalar stays bare on collapse
	// (collapsedArm's non-object guard).
	if m := collapse(t, desc, `{"email":"plain"}`, nil); m["contact"] != "plain" {
		t.Errorf("collapse scalar-as-message arm: %+v", m)
	}
}

// TestAlias_WireKeyResolution covers resolveAlias / wireKey / fieldByWireKey's
// alias arms: with an AliasFunc that renames a field, collapse finds the arm
// key under its alias and a child key resolves via the alias loop.
func TestAlias_WireKeyResolution(t *testing.T) {
	desc := richDesc(t)
	aliasOf := func(fd protoreflect.FieldDescriptor) string {
		if fd.Name() == "phone" {
			return "telephone"
		}
		if fd.Name() == "child" {
			return "kid"
		}
		return ""
	}
	// Arm rendered under its alias collapses to the oneof key.
	if m := collapse(t, desc, `{"telephone":"555"}`, aliasOf); m["contact"] != "555" {
		t.Errorf("aliased arm collapse: %+v", m)
	}
	// Child field under its alias still recurses (fieldByWireKey alias loop);
	// the alias applies globally, so the inner phone arm is "telephone" too.
	m := collapse(t, desc, `{"kid":{"telephone":"9"}}`, aliasOf)
	kid, ok := m["kid"].(map[string]any)
	if !ok || kid["contact"] != "9" {
		t.Errorf("aliased child collapse: %+v", m)
	}
}
