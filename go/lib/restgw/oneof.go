package restgw

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/wandering-compiler/sdk/go/lib/protojsonx"
)

// w17 JSON dialect — FE-friendly oneof shape. The reshape itself lives
// in the neutral lib/protojsonx so every JSON surface (REST/SSE/WS here,
// MCP, …) shares one dialect. restgw wires it into MarshalProto /
// UnmarshalProto, composing it with the rest_alias rewrite by handing
// protojsonx its alias resolver (aliasFor) — the collapse runs AFTER the
// alias rewrite, so variant keys are matched alias-aware.

// DiscriminatorKey re-exports the dialect's discriminator property.
const DiscriminatorKey = protojsonx.DiscriminatorKey

// hasAnyOneof reports whether desc's tree carries a genuine oneof.
func hasAnyOneof(desc protoreflect.MessageDescriptor) bool {
	return protojsonx.HasAnyOneof(desc)
}

// collapseOneofsOnResponseJSON / expandOneofsOnRequestJSON adapt the
// neutral reshape to restgw's alias-aware ordering (aliasFor passed as
// the resolver).
func collapseOneofsOnResponseJSON(raw []byte, desc protoreflect.MessageDescriptor) ([]byte, error) {
	return protojsonx.CollapseOneofs(raw, desc, aliasFor)
}

func expandOneofsOnRequestJSON(raw []byte, desc protoreflect.MessageDescriptor) ([]byte, error) {
	return protojsonx.ExpandOneofs(raw, desc, aliasFor)
}
