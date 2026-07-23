package restgw

import (
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// G3i3-Misc-A — per-field REST alias support.
//
// `(w17.field).rest_alias` renames a proto field on the REST
// surface only. The proto / gRPC wire keeps the original
// name; the gateway swaps it to the alias before writing
// JSON responses and accepts both names on request decode.
//
// This file hosts the alias-aware response writer (used
// implicitly via `MarshalProto` whenever the descriptor
// graph carries any alias) and the request-side
// `restoreAliasesOnRequestJSON` helper that rewrites alias
// keys back to proto names so downstream protojson decode
// finds the fields.
//
// Performance: alias presence is cached per
// `protoreflect.MessageDescriptor` in `aliasInfoCache`
// (sync.Map) — first use per type pays one descriptor
// scan, subsequent calls hit the cache. Messages without
// aliases skip the rewrite path entirely; the byte-for-byte
// output matches the pre-feature path.

// aliasInfoCache memoises whether a message descriptor's
// type tree carries any (w17.field).rest_alias annotation +
// the alias map keyed by descriptor name. Sync.Map shape so
// concurrent first-touch from multiple handlers doesn't
// serialize.
var aliasInfoCache sync.Map // protoreflect.MessageDescriptor → *aliasInfo

type aliasInfo struct {
	// hasAliases is the fast-path predicate. False = no
	// reachable field carries rest_alias; the alias rewrite
	// is a no-op and gets skipped.
	hasAliases bool
}

// hasAnyAlias walks `desc` + every reachable nested message
// descriptor, looking for fields with `(w17.field).rest_alias`
// set. Result is cached per descriptor pointer (descriptors
// are de-duped by the proto runtime so this is stable across
// goroutines).
func hasAnyAlias(desc protoreflect.MessageDescriptor) bool {
	if desc == nil {
		return false
	}
	if cached, ok := aliasInfoCache.Load(desc); ok {
		return cached.(*aliasInfo).hasAliases
	}
	visited := map[protoreflect.MessageDescriptor]bool{}
	result := walkForAliases(desc, visited)
	aliasInfoCache.Store(desc, &aliasInfo{hasAliases: result})
	return result
}

func walkForAliases(desc protoreflect.MessageDescriptor, visited map[protoreflect.MessageDescriptor]bool) bool {
	if desc == nil || visited[desc] {
		return false
	}
	visited[desc] = true
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if aliasFor(fd) != "" {
			return true
		}
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			if walkForAliases(fd.Message(), visited) {
				return true
			}
		}
	}
	return false
}

// aliasFor reads the `rest_alias` value from a field's
// (w17.field) annotation. Returns "" when unset OR when the
// field has no (w17.field) at all.
func aliasFor(fd protoreflect.FieldDescriptor) string {
	if fd == nil {
		return ""
	}
	opts := fd.Options()
	if opts == nil {
		return ""
	}
	if !proto.HasExtension(opts, w17pb.E_Field) {
		return ""
	}
	field, ok := proto.GetExtension(opts, w17pb.E_Field).(*w17pb.Field)
	if !ok || field == nil {
		return ""
	}
	return field.GetRestAlias()
}

// rewriteAliasesOnResponseJSON walks the JSON tree using
// `desc` as a schema and renames every aliased field key.
// Untouched fields keep their proto names. Map fields,
// repeated fields, and nested messages all recurse correctly.
//
// Caller is responsible for deciding whether to invoke this
// (use [hasAnyAlias] as the gate) — passing a descriptor
// without aliases is harmless but pays the parse / re-marshal
// cost for nothing.
func rewriteAliasesOnResponseJSON(raw []byte, desc protoreflect.MessageDescriptor) ([]byte, error) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	out := renameForResponse(node, desc)
	return json.Marshal(out)
}

func renameForResponse(node any, desc protoreflect.MessageDescriptor) any {
	obj, ok := node.(map[string]any)
	if !ok || desc == nil {
		return node
	}
	out := make(map[string]any, len(obj))
	fields := desc.Fields()
	for k, v := range obj {
		var fd protoreflect.FieldDescriptor
		// Look up by proto name first, then JSON name (the
		// project marshaller is configured with UseProtoNames=
		// true so proto name is the common case).
		if fd = fields.ByName(protoreflect.Name(k)); fd == nil {
			fd = fields.ByJSONName(k)
		}
		if fd == nil {
			out[k] = v
			continue
		}
		alias := aliasFor(fd)
		key := k
		if alias != "" {
			key = alias
		}
		out[key] = renameChild(v, fd)
	}
	return out
}

func renameChild(v any, fd protoreflect.FieldDescriptor) any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		// Map fields render as JSON objects with the map's
		// key as the JSON key — never a proto field name —
		// so descend with no descriptor (the map's value
		// type may itself be a message; recurse via the
		// MapValue descriptor).
		if fd.IsMap() {
			obj, ok := v.(map[string]any)
			if !ok {
				return v
			}
			valueFd := fd.MapValue()
			out := make(map[string]any, len(obj))
			for k, vv := range obj {
				out[k] = renameChild(vv, valueFd)
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
				out[i] = renameForResponse(item, fd.Message())
			}
			return out
		}
		return renameForResponse(v, fd.Message())
	default:
		return v
	}
}

// restoreAliasesOnRequestJSON is the symmetric operation for
// inbound request decode: when an alias is set, accept BOTH
// the alias and the proto name on the wire. We rewrite alias
// keys back to proto names before handing the JSON to
// protojson.Unmarshal so the unmarshaller (which only knows
// proto / json names) finds every field.
//
// Returns the rewritten bytes (or the input unchanged when
// no aliases are reachable from `desc`).
func restoreAliasesOnRequestJSON(raw []byte, desc protoreflect.MessageDescriptor) ([]byte, error) {
	if !hasAnyAlias(desc) {
		return raw, nil
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return raw, nil // let protojson surface the parse error with its own context
	}
	out, err := restoreForRequest(node, desc)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func restoreForRequest(node any, desc protoreflect.MessageDescriptor) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok || desc == nil {
		return node, nil
	}
	// Build alias→proto-name reverse map for the current type.
	reverse := map[string]string{}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if alias := aliasFor(fd); alias != "" {
			reverse[alias] = string(fd.Name())
		}
	}
	// Q66-restgw-1: reject a request that supplies BOTH a field's rest_alias
	// AND its canonical proto / json name. Both rewrite to the same output
	// key, so merging them depends on map-iteration order (randomized) — the
	// surviving value would be nondeterministic. Ambiguous input is a client
	// error, so fail loudly instead of silently picking one at random.
	for alias, proto := range reverse {
		if _, hasAlias := obj[alias]; !hasAlias {
			continue
		}
		if _, dup := obj[proto]; dup {
			return nil, fmt.Errorf("field %q given twice: as its rest_alias %q and its proto name %q", proto, alias, proto)
		}
		if pfd := fields.ByName(protoreflect.Name(proto)); pfd != nil {
			if jn := pfd.JSONName(); jn != proto {
				if _, dup := obj[jn]; dup {
					return nil, fmt.Errorf("field %q given twice: as its rest_alias %q and its json name %q", proto, alias, jn)
				}
			}
		}
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		key := k
		if proto, ok := reverse[k]; ok {
			key = proto
		}
		fd := fields.ByName(protoreflect.Name(key))
		if fd == nil {
			fd = fields.ByJSONName(key)
		}
		if fd == nil {
			out[key] = v
			continue
		}
		child, err := restoreChild(v, fd)
		if err != nil {
			return nil, err
		}
		out[key] = child
	}
	return out, nil
}

func restoreChild(v any, fd protoreflect.FieldDescriptor) (any, error) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if fd.IsMap() {
			obj, ok := v.(map[string]any)
			if !ok {
				return v, nil
			}
			valueFd := fd.MapValue()
			out := make(map[string]any, len(obj))
			for k, vv := range obj {
				cv, err := restoreChild(vv, valueFd)
				if err != nil {
					return nil, err
				}
				out[k] = cv
			}
			return out, nil
		}
		if fd.IsList() {
			arr, ok := v.([]any)
			if !ok {
				return v, nil
			}
			out := make([]any, len(arr))
			for i, item := range arr {
				cv, err := restoreForRequest(item, fd.Message())
				if err != nil {
					return nil, err
				}
				out[i] = cv
			}
			return out, nil
		}
		return restoreForRequest(v, fd.Message())
	default:
		return v, nil
	}
}
