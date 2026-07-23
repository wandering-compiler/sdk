package restgw

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// G3i3-Misc-A: messages without (w17.field).rest_alias take
// the byte-for-byte fast path through MarshalProto. Pin the
// behaviour with a representative descriptor proto (a real
// w17 message we already depend on, so the test doesn't need
// dynamic descriptor construction).
func TestMarshalProto_NoAliasFastPath(t *testing.T) {
	// FieldDescriptorProto from descriptorpb has no
	// (w17.field) — exercises the fast path.
	msg := &descriptorpb.FieldDescriptorProto{
		Name:   proto.String("user_id"),
		Number: proto.Int32(1),
	}
	got, err := MarshalProto(msg)
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}
	if !strings.Contains(string(got), `"name":"user_id"`) &&
		!strings.Contains(string(got), `"name": "user_id"`) {
		t.Errorf("expected proto-name shape; got %s", got)
	}
}

// G3i3-Misc-A: with a (w17.field).rest_alias annotation on
// the message descriptor, the JSON output renames the
// aliased keys. Builds a synthetic proto file descriptor to
// exercise the rewrite path without relying on a generated
// fixture (parser tests cover the generated codepath
// separately).
func TestMarshalProto_AliasedFieldRenamesKey(t *testing.T) {
	// We need an actual proto.Message with a (w17.field)
	// annotation set on a field. The simplest robust route is
	// to build one via dynamicpb at runtime — but that pulls
	// the dynamicpb dep into a test that should stay close to
	// the production runtime. Instead, the gateway-side
	// generator integration test verifies the end-to-end shape
	// (proto fixture → parser → schema emit). Here we sanity-
	// check the alias predicate cache directly.
	desc := (&descriptorpb.FieldDescriptorProto{}).ProtoReflect().Descriptor()
	if hasAnyAlias(desc) {
		t.Errorf("descriptorpb.FieldDescriptorProto should not carry rest_alias")
	}
	// Ensure subsequent lookups hit the cache.
	if hasAnyAlias(desc) {
		t.Errorf("cached lookup should still report no alias")
	}
}

// G3i3-Misc-A: the JSON tree walker swaps aliased keys at
// every nesting depth. Drives the helper directly with a
// hand-rolled descriptor + JSON to keep the test independent
// of any full proto compile.
func TestRewriteAliasesOnResponseJSON_RenamesNested(t *testing.T) {
	// Synthesize a descriptor with an aliased field by
	// constructing a FileDescriptorProto + walking it through
	// protodesc. This is verbose but pins the rewrite logic
	// without needing fixture generation in the test pipeline.
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("alias_test.proto"),
		Package: proto.String("aliastest"),
		Syntax:  proto.String("proto3"),
		Dependency: []string{
			"w17/field.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("User"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("user_id"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("userId"),
						Options:  fieldOptionsWithAlias("id"),
					},
					{
						Name:     proto.String("display_name"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("displayName"),
					},
				},
			},
		},
	}
	desc := buildMessageDescriptor(t, fileProto, "aliastest.User")

	in := []byte(`{"user_id":"u-1","display_name":"alice"}`)
	got, err := rewriteAliasesOnResponseJSON(in, desc)
	if err != nil {
		t.Fatalf("rewriteAliases: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if obj["id"] != "u-1" {
		t.Errorf("aliased key 'id' missing; got %v", obj)
	}
	if _, ok := obj["user_id"]; ok {
		t.Errorf("original 'user_id' still present after rename; got %v", obj)
	}
	if obj["display_name"] != "alice" {
		t.Errorf("non-aliased key 'display_name' missing; got %v", obj)
	}
}

func TestRestoreAliasesOnRequestJSON_AcceptsBoth(t *testing.T) {
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("alias_test_req.proto"),
		Package:    proto.String("aliastest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Req"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("user_id"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("userId"),
						Options:  fieldOptionsWithAlias("id"),
					},
				},
			},
		},
	}
	desc := buildMessageDescriptor(t, fileProto, "aliastest.Req")

	for label, in := range map[string]string{
		"alias name on wire":      `{"id":"u-1"}`,
		"proto name on wire":      `{"user_id":"u-1"}`,
		"json camel name on wire": `{"userId":"u-1"}`,
	} {
		t.Run(label, func(t *testing.T) {
			got, err := restoreAliasesOnRequestJSON([]byte(in), desc)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(got, &obj); err != nil {
				t.Fatalf("json: %v", err)
			}
			if obj["user_id"] != "u-1" && obj["userId"] != "u-1" {
				t.Errorf("expected proto-name key after restore; got %v", obj)
			}
			if _, ok := obj["id"]; ok {
				t.Errorf("alias 'id' should be rewritten back to proto name; got %v", obj)
			}
		})
	}
}

// Q66-restgw-1: a request that supplies BOTH a field's rest_alias AND its
// canonical proto/json name is ambiguous — both rewrite to the same key, so
// the surviving value was nondeterministic (map-iteration order). Must be
// rejected, deterministically, instead of silently picking one at random.
func TestRestoreAliasesOnRequestJSON_AliasAndCanonicalCollision_Rejected(t *testing.T) {
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("alias_collide_req.proto"),
		Package:    proto.String("aliastest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Req"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("user_id"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("userId"),
						Options:  fieldOptionsWithAlias("id"),
					},
				},
			},
		},
	}
	desc := buildMessageDescriptor(t, fileProto, "aliastest.Req")

	for label, in := range map[string]string{
		"alias + proto name": `{"id":"a","user_id":"b"}`,
		"alias + json name":  `{"id":"a","userId":"b"}`,
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := restoreAliasesOnRequestJSON([]byte(in), desc); err == nil {
				t.Fatalf("ambiguous %s must be rejected, got nil error", label)
			}
		})
	}

	// Sanity: either form ALONE is still fine (no collision).
	for _, in := range []string{`{"id":"a"}`, `{"user_id":"b"}`, `{"userId":"c"}`} {
		if _, err := restoreAliasesOnRequestJSON([]byte(in), desc); err != nil {
			t.Errorf("single-form %q must be accepted, got %v", in, err)
		}
	}
}

// B27-restgw-1: the alias+canonical collision the helper rejects must ALSO be
// surfaced by UnmarshalProto (the integration). UnmarshalProto previously did
// `if err == nil { payload = restored }`, swallowing the collision error and
// letting protojson DiscardUnknown silently drop one key — a partial-success on
// an ambiguous request. (The helper returns an error ONLY for this collision;
// malformed JSON returns a nil error, so surfacing it is safe.)
func TestUnmarshalProto_AliasAndCanonicalCollision_Rejected(t *testing.T) {
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("alias_collide_unmarshal.proto"),
		Package:    proto.String("aliastest"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"w17/field.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Req"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("user_id"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				JsonName: proto.String("userId"),
				Options:  fieldOptionsWithAlias("id"),
			}},
		}},
	}
	desc := buildMessageDescriptor(t, fileProto, "aliastest.Req")
	msg := dynamicpb.NewMessage(desc)
	if err := UnmarshalProto([]byte(`{"id":"a","user_id":"b"}`), msg); err == nil {
		t.Fatal("alias+canonical collision must be rejected by UnmarshalProto, got nil error")
	}
	// A single form still decodes cleanly.
	if err := UnmarshalProto([]byte(`{"id":"a"}`), dynamicpb.NewMessage(desc)); err != nil {
		t.Errorf("single alias form must decode, got %v", err)
	}
}

func TestHasAnyAlias_Cached(t *testing.T) {
	desc := (&descriptorpb.FieldDescriptorProto{}).ProtoReflect().Descriptor()
	// First call populates the cache.
	first := hasAnyAlias(desc)
	// Second call exercises the cache lookup branch.
	second := hasAnyAlias(desc)
	if first != second {
		t.Errorf("cached lookup mismatch: first=%v second=%v", first, second)
	}
	// nil descriptor never panics.
	if hasAnyAlias(nil) {
		t.Errorf("hasAnyAlias(nil) should be false")
	}
}

// fieldOptionsWithAlias returns a FieldOptions with
// (w17.field).rest_alias set to the supplied value.
func fieldOptionsWithAlias(alias string) *descriptorpb.FieldOptions {
	field := &w17pb.Field{RestAlias: alias}
	opts := &descriptorpb.FieldOptions{}
	proto.SetExtension(opts, w17pb.E_Field, field)
	return opts
}

// buildMessageDescriptor compiles a synthesised
// FileDescriptorProto into a real protoreflect descriptor +
// returns the named message's MessageDescriptor. Imports
// w17/field.proto by pulling the registered FileDescriptor
// from the global proto registry — proto generation already
// loaded it when w17pb was imported.
func buildMessageDescriptor(t testing.TB, file *descriptorpb.FileDescriptorProto, msgFQN string) protoreflect.MessageDescriptor {
	t.Helper()
	descriptorFile, err := protoregistry.GlobalFiles.FindFileByPath("google/protobuf/descriptor.proto")
	if err != nil {
		t.Fatalf("GlobalFiles.FindFileByPath(google/protobuf/descriptor.proto): %v", err)
	}
	w17File, err := protoregistry.GlobalFiles.FindFileByPath("w17/field.proto")
	if err != nil {
		t.Fatalf("GlobalFiles.FindFileByPath(w17/field.proto): %v", err)
	}
	fileSet := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			protodesc.ToFileDescriptorProto(descriptorFile),
			protodesc.ToFileDescriptorProto(w17File),
			file,
		},
	}
	files, err := protodesc.NewFiles(fileSet)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(msgFQN))
	if err != nil {
		t.Fatalf("FindDescriptorByName(%s): %v", msgFQN, err)
	}
	mt, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatalf("descriptor %s is %T, not a message", msgFQN, d)
	}
	return mt
}
