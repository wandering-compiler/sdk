// Package protojsonx is the w17 JSON dialect — protojson
// post-processing shared by every surface that emits JSON of proto
// messages (REST + SSE + WS via restgw, MCP tool results, …). Keeping
// it neutral (no REST / MCP dependency) lets all of them route their
// proto→JSON through ONE dialect, so an SSE event payload, a unary REST
// response, and an MCP tool result look identical on the wire.
//
// The dialect's one behavioural transform is the FE-friendly **oneof
// collapse**: protojson renders a protobuf `oneof` as N flat optional
// keys with no signal of which arm is set — hostile to a frontend that
// wants a single tagged value to `switch` on. This package COLLAPSES the
// flat output into one key (named after the oneof) carrying the set arm,
// message arms tagged with [DiscriminatorKey] and scalar arms left bare;
// and EXPANDS that shape back to flat keys before protojson decodes a
// request. See docs/specs/gateway/json-dialect.md.
//
// proto3 `optional` is a synthetic single-field oneof
// ([protoreflect.OneofDescriptor.IsSynthetic]) and is NOT collapsed.
package protojsonx

import (
	"encoding/json"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// DiscriminatorKey is the property injected into a message-typed oneof
// arm naming which variant is set (its proto field name). Bare
// scalar/enum arms carry no discriminator — they are emitted as the raw
// value and matched back by JSON primitive type on decode.
const DiscriminatorKey = "w17_discriminator"

// AliasFunc resolves a field's REST alias (the `(w17.field).rest_alias`
// it renders under), or "" when unset. Surfaces that apply an alias
// rewrite (restgw) pass their resolver so the collapse — which runs
// AFTER that rewrite — finds the variant key whatever casing it ended up
// in; surfaces with no aliases (MCP) pass nil.
type AliasFunc func(protoreflect.FieldDescriptor) string

func resolveAlias(aliasOf AliasFunc, fd protoreflect.FieldDescriptor) string {
	if aliasOf == nil {
		return ""
	}
	return aliasOf(fd)
}

// oneofInfoCache memoises whether a message descriptor's type tree
// carries any genuine oneof. Keyed by descriptor pointer (de-duped by
// the proto runtime), sync.Map so concurrent first-touch doesn't
// serialize.
var oneofInfoCache sync.Map // protoreflect.MessageDescriptor → bool

// HasAnyOneof reports whether desc or any reachable nested message
// declares a genuine (non-synthetic) oneof. Result cached per
// descriptor. Callers gate the collapse/expand on this so a message
// without a genuine oneof stays byte-identical to plain protojson.
func HasAnyOneof(desc protoreflect.MessageDescriptor) bool {
	if desc == nil {
		return false
	}
	if cached, ok := oneofInfoCache.Load(desc); ok {
		return cached.(bool)
	}
	result := walkForOneofs(desc, map[protoreflect.MessageDescriptor]bool{})
	oneofInfoCache.Store(desc, result)
	return result
}

func walkForOneofs(desc protoreflect.MessageDescriptor, visited map[protoreflect.MessageDescriptor]bool) bool {
	if desc == nil || visited[desc] {
		return false
	}
	visited[desc] = true
	oneofs := desc.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		if !oneofs.Get(i).IsSynthetic() {
			return true
		}
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			if walkForOneofs(fd.Message(), visited) {
				return true
			}
		}
	}
	return false
}

// wireKey returns the JSON key a field renders under (proto name, or its
// rest_alias when the caller applied one before collapse).
func wireKey(fd protoreflect.FieldDescriptor, aliasOf AliasFunc) string {
	if a := resolveAlias(aliasOf, fd); a != "" {
		return a
	}
	return string(fd.Name())
}

// fieldByWireKey resolves the field a JSON key maps to, tolerant of
// proto name, JSON (camel) name, and rest_alias. Returns nil for keys
// matching no field (an already-collapsed oneof key, or unknown field).
func fieldByWireKey(desc protoreflect.MessageDescriptor, key string, aliasOf AliasFunc) protoreflect.FieldDescriptor {
	fields := desc.Fields()
	if fd := fields.ByName(protoreflect.Name(key)); fd != nil {
		return fd
	}
	if fd := fields.ByJSONName(key); fd != nil {
		return fd
	}
	if aliasOf != nil {
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if aliasOf(fd) == key {
				return fd
			}
		}
	}
	return nil
}

// scalarJSONKind classifies how protojson renders a scalar field's JSON
// value — "string", "number", or "boolean" — so a bare oneof arm can be
// matched back to its variant by type. 64-bit integers, enums and bytes
// all render as JSON strings; 32-bit ints and floats as numbers. Message
// kinds return "" (they carry the discriminator instead).
func scalarJSONKind(fd protoreflect.FieldDescriptor) string {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return "boolean"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return "number"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.StringKind, protoreflect.EnumKind, protoreflect.BytesKind:
		return "string"
	case protoreflect.MessageKind, protoreflect.GroupKind:
		// A scalar-serializing WKT bare oneof arm routes by the JSON
		// primitive it renders as (Timestamp/Duration/FieldMask/string
		// wrappers → string; number wrappers → number; BoolValue → boolean).
		return wktScalarJSONKind(fd.Message().FullName())
	default:
		return ""
	}
}

// wktScalarJSONKind maps a scalar-serializing WKT to its JSON primitive
// kind, or "" for object-shaped WKTs (Struct/Value/ListValue/Any) and
// non-WKT messages (which are matched differently — message arms by
// discriminator, object-WKT arms by isObjectWKT).
func wktScalarJSONKind(name protoreflect.FullName) string {
	switch name {
	case "google.protobuf.Timestamp", "google.protobuf.Duration",
		"google.protobuf.FieldMask", "google.protobuf.StringValue",
		"google.protobuf.BytesValue", "google.protobuf.Int64Value",
		"google.protobuf.UInt64Value":
		return "string"
	case "google.protobuf.Int32Value", "google.protobuf.UInt32Value",
		"google.protobuf.FloatValue", "google.protobuf.DoubleValue":
		return "number"
	case "google.protobuf.BoolValue":
		return "boolean"
	default:
		return ""
	}
}

// IsDiscriminatorArm reports whether a genuine-oneof member field is a
// "tagged" arm — i.e. it serializes as a JSON object and therefore carries
// the [DiscriminatorKey] const in the collapsed dialect, rather than being
// matched bare by JSON primitive type. True only for a genuine (non-WKT)
// message arm; scalars, scalar-serializing WKTs, and object-shaped WKTs
// (Struct/Value/ListValue/Any) all stay bare. Exported so schema generators
// (MCP inputSchema, OpenAPI) advertise exactly the request shape ExpandOneofs
// accepts — a single source of truth for the bare-vs-tagged decision.
func IsDiscriminatorArm(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return false
	}
	return scalarJSONKind(fd) == "" && !isObjectWKTArm(fd)
}

// BareArmJSONKind returns the JSON primitive ("string"/"number"/"boolean") a
// BARE oneof arm is matched back by at decode time (ExpandOneofs), or "" for a
// discriminator-tagged message arm and for an object-shaped WKT arm (matched by
// object shape, not primitive). Two bare arms that share a non-empty kind are
// ambiguous — ExpandOneofs can't tell them apart — so schema generators use
// this to reject such a oneof at build time (single source of truth with the
// runtime routing). Synthetic with scalarJSONKind, exported for reuse.
func BareArmJSONKind(fd protoreflect.FieldDescriptor) string {
	if IsDiscriminatorArm(fd) {
		return ""
	}
	return scalarJSONKind(fd)
}

// isObjectWKTArm reports whether a oneof arm field is an object-shaped WKT
// (Struct/Value/ListValue/Any) — these collapse to a bare object/array
// with no discriminator, so they are matched as the unique object arm.
func isObjectWKTArm(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return false
	}
	switch fd.Message().FullName() {
	case "google.protobuf.Struct", "google.protobuf.Value",
		"google.protobuf.ListValue", "google.protobuf.Any":
		return true
	}
	return false
}

// jsonValueKind reports the JSON primitive kind of a decoded value
// ("string"/"number"/"boolean"), or "" for objects/arrays/null.
func jsonValueKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "boolean"
	default:
		return ""
	}
}

// CollapseOneofs rewrites protojson's flat oneof keys into the collapsed
// dialect shape. Caller gates on [HasAnyOneof]. aliasOf is the alias
// resolver to match variant keys with (nil when no alias rewrite ran).
func CollapseOneofs(raw []byte, desc protoreflect.MessageDescriptor, aliasOf AliasFunc) ([]byte, error) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	return json.Marshal(collapseResponse(node, desc, aliasOf))
}

func collapseResponse(node any, desc protoreflect.MessageDescriptor, aliasOf AliasFunc) any {
	obj, ok := node.(map[string]any)
	if !ok || desc == nil {
		return node
	}
	// Recurse into children first so oneofs nested inside variant objects
	// collapse before we lift them.
	work := make(map[string]any, len(obj))
	for k, v := range obj {
		work[k] = collapseChild(v, fieldByWireKey(desc, k, aliasOf), aliasOf)
	}
	oneofs := desc.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		if od.IsSynthetic() {
			continue
		}
		flds := od.Fields()
		for j := 0; j < flds.Len(); j++ {
			fd := flds.Get(j)
			wk := wireKey(fd, aliasOf)
			val, present := work[wk]
			if !present {
				continue
			}
			delete(work, wk)
			work[string(od.Name())] = collapsedArm(fd, val)
			break // at most one arm is set
		}
	}
	return work
}

func collapsedArm(fd protoreflect.FieldDescriptor, val any) any {
	if !IsDiscriminatorArm(fd) {
		// Only a genuine (non-WKT) message arm is tagged. Scalars,
		// scalar-serializing WKTs (Timestamp/Duration/…) AND object-shaped
		// WKTs (Struct/Value/ListValue/Any) all stay bare — matched back on
		// decode by JSON primitive type or unique object shape, never by an
		// injected discriminator. This mirrors IsDiscriminatorArm exactly, so
		// the wire shape matches what the OpenAPI/MCP schema generators
		// advertise; tagging an arbitrary-JSON Struct would break that bare
		// contract and clobber a user key literally named w17_discriminator.
		return val
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return val // defensive: a message arm arriving as a bare scalar stays bare
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out[DiscriminatorKey] = string(fd.Name())
	return out
}

func collapseChild(v any, fd protoreflect.FieldDescriptor, aliasOf AliasFunc) any {
	if fd == nil {
		return v
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return v
	}
	if fd.IsMap() {
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		valFd := fd.MapValue()
		if valFd.Kind() != protoreflect.MessageKind && valFd.Kind() != protoreflect.GroupKind {
			return v
		}
		out := make(map[string]any, len(obj))
		for k, vv := range obj {
			out[k] = collapseResponse(vv, valFd.Message(), aliasOf)
		}
		return out
	}
	if fd.IsList() {
		arr, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(arr))
		for i, item := range arr {
			out[i] = collapseResponse(item, fd.Message(), aliasOf)
		}
		return out
	}
	return collapseResponse(v, fd.Message(), aliasOf)
}

// ExpandOneofs rewrites a collapsed-oneof request body back to
// protojson's flat shape so protojson can decode it. Caller gates on
// [HasAnyOneof]. On malformed JSON returns the input unchanged.
func ExpandOneofs(raw []byte, desc protoreflect.MessageDescriptor, aliasOf AliasFunc) ([]byte, error) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return raw, nil
	}
	return json.Marshal(expandRequest(node, desc, aliasOf))
}

func expandRequest(node any, desc protoreflect.MessageDescriptor, aliasOf AliasFunc) any {
	obj, ok := node.(map[string]any)
	if !ok || desc == nil {
		return node
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if od := genuineOneofByKey(desc, k); od != nil {
			if fd, arm := resolveArm(od, v); fd != nil {
				out[string(fd.Name())] = expandChild(arm, fd, aliasOf)
				continue
			}
			out[k] = v
			continue
		}
		out[k] = expandChild(v, fieldByWireKey(desc, k, aliasOf), aliasOf)
	}
	return out
}

func genuineOneofByKey(desc protoreflect.MessageDescriptor, key string) protoreflect.OneofDescriptor {
	oneofs := desc.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		if od.IsSynthetic() {
			continue
		}
		name := string(od.Name())
		if name == key || snakeToCamel(name) == key {
			return od
		}
	}
	return nil
}

func resolveArm(od protoreflect.OneofDescriptor, v any) (protoreflect.FieldDescriptor, any) {
	flds := od.Fields()
	if obj, ok := v.(map[string]any); ok {
		if disc, ok := obj[DiscriminatorKey].(string); ok {
			fd := flds.ByName(protoreflect.Name(disc))
			if fd == nil {
				return nil, nil
			}
			stripped := make(map[string]any, len(obj))
			for k, vv := range obj {
				if k == DiscriminatorKey {
					continue
				}
				stripped[k] = vv
			}
			return fd, stripped
		}
	}
	if kind := jsonValueKind(v); kind != "" {
		// Bare scalar (incl scalar-WKT) → the arm of that JSON primitive.
		for j := 0; j < flds.Len(); j++ {
			fd := flds.Get(j)
			if scalarJSONKind(fd) == kind {
				return fd, v
			}
		}
		return nil, nil
	}
	// Bare object/array with no discriminator → the unique object-shaped
	// WKT arm (Struct/Value/ListValue/Any). Unique by construction: a
	// genuine message arm always carries a discriminator, so it can't be
	// confused with these.
	var objArm protoreflect.FieldDescriptor
	for j := 0; j < flds.Len(); j++ {
		if fd := flds.Get(j); isObjectWKTArm(fd) {
			if objArm != nil {
				return nil, nil // ambiguous: >1 object-WKT arm
			}
			objArm = fd
		}
	}
	if objArm != nil {
		return objArm, v
	}
	return nil, nil
}

func expandChild(v any, fd protoreflect.FieldDescriptor, aliasOf AliasFunc) any {
	if fd == nil {
		return v
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return v
	}
	if fd.IsMap() {
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		valFd := fd.MapValue()
		if valFd.Kind() != protoreflect.MessageKind && valFd.Kind() != protoreflect.GroupKind {
			return v
		}
		out := make(map[string]any, len(obj))
		for k, vv := range obj {
			out[k] = expandRequest(vv, valFd.Message(), aliasOf)
		}
		return out
	}
	if fd.IsList() {
		arr, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(arr))
		for i, item := range arr {
			out[i] = expandRequest(item, fd.Message(), aliasOf)
		}
		return out
	}
	return expandRequest(v, fd.Message(), aliasOf)
}

// snakeToCamel converts a proto snake_case name to lowerCamelCase
// (protojson's JSON-name derivation) so the request expander tolerates a
// camelCased oneof key from a JS client.
func snakeToCamel(s string) string {
	var b []byte
	up := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		b = append(b, c)
	}
	return string(b)
}
