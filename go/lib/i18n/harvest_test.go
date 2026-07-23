package i18n_test

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// TestHarvestEnumLabels_TopLevelEnum — non-empty
// `(w17.value_display).label` annotations on a top-level enum
// land in the deduplicated sorted output.
func TestHarvestEnumLabels_TopLevelEnum(t *testing.T) {
	f := makeFileWithEnum("status_enum", []enumValue{
		{Name: "ACTIVE", Number: 1, Label: "Active"},
		{Name: "PAUSED", Number: 2, Label: "Paused"},
		{Name: "BLANK", Number: 3, Label: ""},
	})
	got := i18n.HarvestEnumLabels([]*descriptorpb.FileDescriptorProto{f})
	want := []string{"Active", "Paused"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HarvestEnumLabels = %v, want %v", got, want)
	}
}

// TestHarvestEnumLabels_DedupsAcrossEnumsAndFiles — same label
// declared on two different enums in two different files
// collapses to one entry.
func TestHarvestEnumLabels_DedupsAcrossEnumsAndFiles(t *testing.T) {
	a := makeFileWithEnum("status_a", []enumValue{
		{Name: "ACTIVE", Number: 1, Label: "Active"},
	})
	b := makeFileWithEnum("status_b", []enumValue{
		{Name: "ON", Number: 1, Label: "Active"},
		{Name: "OFF", Number: 2, Label: "Inactive"},
	})
	got := i18n.HarvestEnumLabels([]*descriptorpb.FileDescriptorProto{a, b})
	want := []string{"Active", "Inactive"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HarvestEnumLabels = %v, want %v", got, want)
	}
}

// TestHarvestEnumLabels_EmptyInput — nil + empty file slices
// both return nil without panicking.
func TestHarvestEnumLabels_EmptyInput(t *testing.T) {
	if got := i18n.HarvestEnumLabels(nil); len(got) != 0 {
		t.Errorf("nil input: got %v, want empty", got)
	}
	if got := i18n.HarvestEnumLabels([]*descriptorpb.FileDescriptorProto{}); len(got) != 0 {
		t.Errorf("empty slice: got %v, want empty", got)
	}
}

// TestMergePO_PreservesTranslatorEdits — msgstrs in the prior
// `.po` survive the merge; new harvested msgids land with the
// language-appropriate default (msgid for EN, empty for others).
func TestMergePO_PreservesTranslatorEdits(t *testing.T) {
	prior, err := i18n.MarshalPO("cs", []i18n.POEntry{
		{Msgid: "Active", Msgstr: "Aktivní"},
		{Msgid: "Inactive", Msgstr: "Neaktivní"},
	})
	if err != nil {
		t.Fatalf("MarshalPO setup: %v", err)
	}
	merged, err := i18n.MergePO(prior, "cs", []string{"Active", "Pending", "Inactive"})
	if err != nil {
		t.Fatalf("MergePO: %v", err)
	}
	body := string(merged)
	// Existing translations preserved.
	for _, want := range []string{
		`msgid "Active"` + "\n" + `msgstr "Aktivní"`,
		`msgid "Inactive"` + "\n" + `msgstr "Neaktivní"`,
		// New msgid added; non-EN scaffolds empty msgstr.
		`msgid "Pending"` + "\n" + `msgstr ""`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merged body missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// TestMergePO_DropsObsoleteMsgids — msgids in prior but not in
// the new set drop silently (iter-1 — no `#~ msgid` markup).
func TestMergePO_DropsObsoleteMsgids(t *testing.T) {
	prior, _ := i18n.MarshalPO("cs", []i18n.POEntry{
		{Msgid: "Active", Msgstr: "Aktivní"},
		{Msgid: "Retired", Msgstr: "Vyřazené"},
	})
	merged, err := i18n.MergePO(prior, "cs", []string{"Active"})
	if err != nil {
		t.Fatalf("MergePO: %v", err)
	}
	body := string(merged)
	if strings.Contains(body, "Retired") {
		t.Errorf("obsolete msgid leaked into merged body:\n%s", body)
	}
	if !strings.Contains(body, `msgid "Active"`+"\n"+`msgstr "Aktivní"`) {
		t.Errorf("Active translation lost:\n%s", body)
	}
}

// TestMarshalJSON_SortedAndStable — emit is sorted by msgid +
// deterministic across runs (golden-test friendly).
func TestMarshalJSON_SortedAndStable(t *testing.T) {
	entries := []i18n.POEntry{
		{Msgid: "Banana", Msgstr: "Banana"},
		{Msgid: "Apple", Msgstr: "Apple"},
		{Msgid: "Cherry", Msgstr: "Cherry"},
	}
	a, err := i18n.MarshalJSON("en", entries)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	b, err := i18n.MarshalJSON("en", entries)
	if err != nil {
		t.Fatalf("MarshalJSON second: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("MarshalJSON output not deterministic")
	}
	body := string(a)
	apple := strings.Index(body, `"Apple"`)
	banana := strings.Index(body, `"Banana"`)
	cherry := strings.Index(body, `"Cherry"`)
	if apple < 0 || banana < 0 || cherry < 0 || !(apple < banana && banana < cherry) {
		t.Errorf("entries not in sorted order; body:\n%s", body)
	}
}

// --- helpers ---

type enumValue struct {
	Name   string
	Number int32
	Label  string
}

func makeFileWithEnum(filename string, values []enumValue) *descriptorpb.FileDescriptorProto {
	name := filename + ".proto"
	enumName := "TestEnum"
	enum := &descriptorpb.EnumDescriptorProto{Name: &enumName}
	for i, v := range values {
		name := v.Name
		num := v.Number
		ev := &descriptorpb.EnumValueDescriptorProto{
			Name:   &name,
			Number: &num,
		}
		if v.Label != "" {
			opts := &descriptorpb.EnumValueOptions{}
			proto.SetExtension(opts, w17pb.E_ValueDisplay, &w17pb.EnumValueDisplay{
				Label: v.Label,
			})
			ev.Options = opts
		}
		_ = i
		enum.Value = append(enum.Value, ev)
	}
	return &descriptorpb.FileDescriptorProto{
		Name:     &name,
		EnumType: []*descriptorpb.EnumDescriptorProto{enum},
	}
}
