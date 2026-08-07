package i18n_test

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// HarvestEnumLabels walks enums nested inside message types and
// recursively inside nested message types — a label declared on a
// deeply-nested enum still lands in the harvested set.
func TestHarvestEnumLabels_NestedMessageEnums(t *testing.T) {
	innerEnumName := "Inner"
	innerVal := "GO"
	innerNum := int32(1)
	innerOpts := &descriptorpb.EnumValueOptions{}
	proto.SetExtension(innerOpts, w17pb.E_ValueDisplay, &w17pb.EnumValueDisplay{Label: "Nested Label"})
	innerEnum := &descriptorpb.EnumDescriptorProto{
		Name: &innerEnumName,
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: &innerVal, Number: &innerNum, Options: innerOpts},
		},
	}

	// Enum declared directly on a message.
	directEnumName := "Direct"
	directVal := "ON"
	directNum := int32(1)
	directOpts := &descriptorpb.EnumValueOptions{}
	proto.SetExtension(directOpts, w17pb.E_ValueDisplay, &w17pb.EnumValueDisplay{Label: "Direct Label"})
	directEnum := &descriptorpb.EnumDescriptorProto{
		Name: &directEnumName,
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: &directVal, Number: &directNum, Options: directOpts},
		},
	}

	nestedMsgName := "Nested"
	nestedMsg := &descriptorpb.DescriptorProto{
		Name:     &nestedMsgName,
		EnumType: []*descriptorpb.EnumDescriptorProto{innerEnum},
	}
	outerMsgName := "Outer"
	outerMsg := &descriptorpb.DescriptorProto{
		Name:       &outerMsgName,
		EnumType:   []*descriptorpb.EnumDescriptorProto{directEnum},
		NestedType: []*descriptorpb.DescriptorProto{nestedMsg},
	}
	fileName := "nested.proto"
	f := &descriptorpb.FileDescriptorProto{
		Name:        &fileName,
		MessageType: []*descriptorpb.DescriptorProto{outerMsg},
	}

	got := i18n.HarvestEnumLabels([]*descriptorpb.FileDescriptorProto{f})
	want := []string{"Direct Label", "Nested Label"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HarvestEnumLabels = %v, want %v", got, want)
	}
}

// An enum value carrying options that LACK the value_display
// extension is skipped (no msgid harvested) — the harvester only
// keys off (w17.value_display).label.
func TestHarvestEnumLabels_OptionsWithoutValueDisplay(t *testing.T) {
	enumName := "NoDisplay"
	valName := "X"
	num := int32(1)
	// Options present (Deprecated set) but no value_display extension.
	deprecated := true
	opts := &descriptorpb.EnumValueOptions{Deprecated: &deprecated}
	enum := &descriptorpb.EnumDescriptorProto{
		Name: &enumName,
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: &valName, Number: &num, Options: opts},
		},
	}
	fileName := "nodisplay.proto"
	f := &descriptorpb.FileDescriptorProto{
		Name:     &fileName,
		EnumType: []*descriptorpb.EnumDescriptorProto{enum},
	}
	if got := i18n.HarvestEnumLabels([]*descriptorpb.FileDescriptorProto{f}); len(got) != 0 {
		t.Errorf("HarvestEnumLabels = %v, want empty (no value_display extension)", got)
	}
}
