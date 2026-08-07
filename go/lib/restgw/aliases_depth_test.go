package restgw

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// aliasNestedDesc builds an Outer message exercising every recursion
// shape the alias rewriter must descend: a singular nested message,
// a repeated message, and a map<string, message>. Inner carries one
// aliased field (secret_field ⇄ "secret") so the rename/restore is
// observable at each nesting depth.
func aliasNestedDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	strField := func(name, json, alias string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(json),
		}
		if alias != "" {
			f.Options = fieldOptionsWithAlias(alias)
		}
		return f
	}
	msgField := func(name string, num int32, repeated bool, typeName string) *descriptorpb.FieldDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		if repeated {
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
		return &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(num),
			Label:    label.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(typeName),
			JsonName: proto.String(name),
		}
	}
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("alias_nested.proto"),
		Package:    proto.String("aliastest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:  proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{strField("secret_field", "secretField", "secret")},
			},
			{
				Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("one", 1, false, ".aliastest.Inner"),
					msgField("many", 2, true, ".aliastest.Inner"),
					{
						Name:     proto.String("meta"),
						Number:   proto.Int32(3),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".aliastest.Outer.MetaEntry"),
						JsonName: proto.String("meta"),
					},
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("MetaEntry"),
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
					Field: []*descriptorpb.FieldDescriptorProto{
						strField("key", "key", ""),
						msgField("value", 2, false, ".aliastest.Inner"),
					},
				}},
			},
		},
	}
	return buildMessageDescriptor(t, fileProto, "aliastest.Outer")
}

// TestRewriteAliasesOnResponseJSON_NestedRepeatedMap pins the response
// rename across all three message-container shapes: secret_field is
// renamed to "secret" inside the singular nested message, every element
// of the repeated message, and every value of the map.
func TestRewriteAliasesOnResponseJSON_NestedRepeatedMap(t *testing.T) {
	desc := aliasNestedDesc(t)
	in := []byte(`{
		"one":  {"secret_field":"x"},
		"many": [{"secret_field":"a"},{"secret_field":"b"}],
		"meta": {"k1":{"secret_field":"m"}}
	}`)
	got, err := rewriteAliasesOnResponseJSON(in, desc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	one := obj["one"].(map[string]any)
	if one["secret"] != "x" {
		t.Errorf("nested message not renamed: %v", one)
	}
	many := obj["many"].([]any)
	if many[0].(map[string]any)["secret"] != "a" || many[1].(map[string]any)["secret"] != "b" {
		t.Errorf("repeated message not renamed: %v", many)
	}
	meta := obj["meta"].(map[string]any)
	if meta["k1"].(map[string]any)["secret"] != "m" {
		t.Errorf("map value message not renamed: %v", meta)
	}
}

// TestRewriteAliasesOnResponseJSON_TypeMismatchPassThrough — when the
// JSON value's runtime type doesn't match the declared message-container
// shape (a map field given a scalar, a list field given a scalar), the
// child is returned verbatim rather than mangled.
func TestRewriteAliasesOnResponseJSON_TypeMismatchPassThrough(t *testing.T) {
	desc := aliasNestedDesc(t)
	in := []byte(`{"one":"scalar","many":"notArray","meta":"notObject"}`)
	got, err := rewriteAliasesOnResponseJSON(in, desc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["one"] != "scalar" || obj["many"] != "notArray" || obj["meta"] != "notObject" {
		t.Errorf("type-mismatched children must pass through unchanged: %v", obj)
	}
}

// TestRewriteAliasesOnResponseJSON_NonObjectRoot — a top-level JSON value
// that isn't an object (e.g. an array) is returned as-is; the renamer
// only descends objects.
func TestRewriteAliasesOnResponseJSON_NonObjectRoot(t *testing.T) {
	desc := aliasNestedDesc(t)
	got, err := rewriteAliasesOnResponseJSON([]byte(`[1,2,3]`), desc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if strings.TrimSpace(string(got)) != `[1,2,3]` {
		t.Errorf("non-object root should pass through; got %s", got)
	}
}

// TestRewriteAliasesOnResponseJSON_MalformedJSON — invalid input surfaces
// the json.Unmarshal error.
func TestRewriteAliasesOnResponseJSON_MalformedJSON(t *testing.T) {
	desc := aliasNestedDesc(t)
	if _, err := rewriteAliasesOnResponseJSON([]byte(`{not json`), desc); err == nil {
		t.Fatal("malformed JSON must return an error")
	}
}

// TestRestoreAliasesOnRequestJSON_NestedRepeatedMap — the inbound restore
// rewrites the alias back to the proto name at every nesting depth.
func TestRestoreAliasesOnRequestJSON_NestedRepeatedMap(t *testing.T) {
	desc := aliasNestedDesc(t)
	in := []byte(`{
		"one":  {"secret":"x"},
		"many": [{"secret":"a"}],
		"meta": {"k1":{"secret":"m"}}
	}`)
	got, err := restoreAliasesOnRequestJSON(in, desc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["one"].(map[string]any)["secret_field"] != "x" {
		t.Errorf("nested not restored: %v", obj["one"])
	}
	if obj["many"].([]any)[0].(map[string]any)["secret_field"] != "a" {
		t.Errorf("repeated not restored: %v", obj["many"])
	}
	if obj["meta"].(map[string]any)["k1"].(map[string]any)["secret_field"] != "m" {
		t.Errorf("map value not restored: %v", obj["meta"])
	}
}

// TestRestoreAliasesOnRequestJSON_TypeMismatchPassThrough — restore also
// leaves type-mismatched container values untouched.
func TestRestoreAliasesOnRequestJSON_TypeMismatchPassThrough(t *testing.T) {
	desc := aliasNestedDesc(t)
	in := []byte(`{"one":"scalar","many":"notArray","meta":"notObject"}`)
	got, err := restoreAliasesOnRequestJSON(in, desc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["one"] != "scalar" || obj["many"] != "notArray" || obj["meta"] != "notObject" {
		t.Errorf("type-mismatched children must pass through: %v", obj)
	}
}

// TestRestoreAliasesOnRequestJSON_NestedCollisionPropagates — an alias +
// canonical-name collision buried inside a nested / repeated / map message
// must propagate out as an error (not be silently merged).
func TestRestoreAliasesOnRequestJSON_NestedCollisionPropagates(t *testing.T) {
	desc := aliasNestedDesc(t)
	cases := map[string]string{
		"nested":   `{"one":{"secret":"a","secret_field":"b"}}`,
		"repeated": `{"many":[{"secret":"a","secret_field":"b"}]}`,
		"map":      `{"meta":{"k":{"secret":"a","secret_field":"b"}}}`,
	}
	for label, in := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := restoreAliasesOnRequestJSON([]byte(in), desc); err == nil {
				t.Fatalf("%s collision must propagate an error, got nil", label)
			}
		})
	}
}

// TestRestoreAliasesOnRequestJSON_NoAliasFastPath — a descriptor with no
// reachable alias returns the input bytes unchanged (no parse/marshal).
func TestRestoreAliasesOnRequestJSON_NoAliasFastPath(t *testing.T) {
	desc := (&descriptorpb.FieldDescriptorProto{}).ProtoReflect().Descriptor()
	in := []byte(`{"name":"x","number":5}`)
	got, err := restoreAliasesOnRequestJSON(in, desc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if string(got) != string(in) {
		t.Errorf("no-alias descriptor must pass bytes through verbatim; got %s", got)
	}
}

// TestRestoreAliasesOnRequestJSON_MalformedJSON — malformed JSON on a
// descriptor that DOES have aliases returns the raw bytes with a nil error,
// deferring the parse error to protojson (which gives better context).
func TestRestoreAliasesOnRequestJSON_MalformedJSON(t *testing.T) {
	desc := aliasNestedDesc(t)
	in := []byte(`{not json`)
	got, err := restoreAliasesOnRequestJSON(in, desc)
	if err != nil {
		t.Fatalf("restore should defer parse error, got %v", err)
	}
	if string(got) != string(in) {
		t.Errorf("malformed input must be returned verbatim for protojson; got %s", got)
	}
}

// TestAliasFor_Guards pins the aliasFor reject arms: a nil field, and a
// field whose options carry no (w17.field) extension both yield "".
func TestAliasFor_Guards(t *testing.T) {
	if got := aliasFor(nil); got != "" {
		t.Errorf("aliasFor(nil) = %q, want empty", got)
	}
	// descriptorpb's own fields have options but no (w17.field).
	fd := (&descriptorpb.FieldDescriptorProto{}).ProtoReflect().Descriptor().Fields().ByName("name")
	if fd == nil {
		t.Fatal("expected a `name` field on FieldDescriptorProto")
	}
	if got := aliasFor(fd); got != "" {
		t.Errorf("aliasFor(no-w17-ext) = %q, want empty", got)
	}
}
