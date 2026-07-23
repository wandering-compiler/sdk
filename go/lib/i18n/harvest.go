package i18n

import (
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// HarvestEnumLabels walks every enum value across `files`
// (top-level + nested in messages, recursively) and returns the
// deduplicated, lexicographically-sorted list of non-empty
// `(w17.value_display).label` strings. The result is intended
// as msgid input to [EmitDomainCatalogs] / [MergePO] so author-
// supplied + acllock-emitter-supplied labels land in the
// project's `.po` catalogs alongside `validation.Defaults()`.
//
// Empty labels (`label: ""` or missing extension entirely) are
// skipped — the admin renderer auto-humanizes from the value
// name in that case, and the auto-humanized fallback is a
// rendering concern, not a translation concern: there's no
// stable msgid to key off of.
//
// Walks BOTH `EnumType` (top-level) and recursively into
// `MessageType[i].NestedType[j]…` so a label declared on a
// nested enum gets harvested even though no real-world proto
// in this project currently nests them. Defensive coverage
// matches what protoc itself walks.
func HarvestEnumLabels(files []*descriptorpb.FileDescriptorProto) []string {
	seen := map[string]bool{}
	for _, f := range files {
		for _, e := range f.GetEnumType() {
			collectEnumLabels(e, seen)
		}
		for _, m := range f.GetMessageType() {
			collectMessageEnumLabels(m, seen)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func collectMessageEnumLabels(m *descriptorpb.DescriptorProto, seen map[string]bool) {
	for _, e := range m.GetEnumType() {
		collectEnumLabels(e, seen)
	}
	for _, nested := range m.GetNestedType() {
		collectMessageEnumLabels(nested, seen)
	}
}

func collectEnumLabels(e *descriptorpb.EnumDescriptorProto, seen map[string]bool) {
	for _, v := range e.GetValue() {
		opts := reparseEnumValueOptions(v.GetOptions())
		if opts == nil {
			continue
		}
		if !proto.HasExtension(opts, w17pb.E_ValueDisplay) {
			continue
		}
		d, _ := proto.GetExtension(opts, w17pb.E_ValueDisplay).(*w17pb.EnumValueDisplay)
		if d == nil {
			continue
		}
		if label := d.GetLabel(); label != "" {
			seen[label] = true
		}
	}
}

// reparseEnumValueOptions re-marshals the options through the
// global extension registry so `(w17.value_display)` decodes
// as the typed message rather than the `*dynamicpb.Message`
// protocompile produces. Mirrors the same trick the gateway
// parser uses (`gateway/parser/options.go::reparseEnumValueOptions`).
func reparseEnumValueOptions(opts *descriptorpb.EnumValueOptions) *descriptorpb.EnumValueOptions {
	if opts == nil {
		return nil
	}
	raw, err := proto.Marshal(opts)
	if err != nil {
		return opts
	}
	dst := &descriptorpb.EnumValueOptions{}
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(raw, dst); err != nil {
		return opts
	}
	return dst
}
