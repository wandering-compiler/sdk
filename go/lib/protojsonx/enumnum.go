package protojsonx

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
)

var enumInfoCache sync.Map // protoreflect.MessageDescriptor → bool

// HasAnyEnum reports whether desc or any reachable nested message declares
// an enum-typed field (singular, repeated or map value). Cached per
// descriptor; callers gate CoerceEnumNumericStrings on it so a message
// without an enum never pays for the tree walk.
func HasAnyEnum(desc protoreflect.MessageDescriptor) bool {
	if desc == nil {
		return false
	}
	if cached, ok := enumInfoCache.Load(desc); ok {
		return cached.(bool)
	}
	result := walkForEnums(desc, map[protoreflect.MessageDescriptor]bool{})
	enumInfoCache.Store(desc, result)
	return result
}

func walkForEnums(desc protoreflect.MessageDescriptor, visited map[protoreflect.MessageDescriptor]bool) bool {
	if desc == nil || visited[desc] || isWKT(desc) {
		return false
	}
	visited[desc] = true
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch {
		case fd.Kind() == protoreflect.EnumKind:
			return true
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			if walkForEnums(fd.Message(), visited) {
				return true
			}
		}
	}
	return false
}

func isWKT(desc protoreflect.MessageDescriptor) bool {
	return strings.HasPrefix(string(desc.FullName()), "google.protobuf.")
}

// CoerceEnumNumericStrings rewrites every enum value that arrives as a
// QUOTED integer ("7") into the bare JSON number protojson reads as that
// enum number. The w17 wire carries an enum as its number, and w17's own
// clients produce the number as a string in two places: the generated
// TS/JS clients keep a value their build does not name as its numeric
// string, and the rpc-cli quotes an enum flag. Stock protojson reads any
// quoted enum as a value NAME and — with DiscardUnknown — silently drops
// one it does not know, so the value was lost with no error.
//
// Only enum-typed fields are touched (an int64 or string "7" stays a
// string), keys are matched by proto or JSON name, and the walk descends
// into nested messages, lists and map values; well-known types are left
// alone. Returns raw unchanged when nothing needed rewriting, or when raw
// is not valid JSON (protojson reports that).
func CoerceEnumNumericStrings(raw []byte, desc protoreflect.MessageDescriptor) ([]byte, error) {
	if !mayHoldQuotedInteger(raw) {
		return raw, nil
	}
	node, err := DecodeTree(raw)
	if err != nil {
		return raw, nil
	}
	if !coerceEnumsIn(node, desc) {
		return raw, nil
	}
	return json.Marshal(node)
}

// mayHoldQuotedInteger is the cheap pre-check: a `"` directly followed by
// a digit or '-'. Bodies without one cannot carry a quoted enum number.
func mayHoldQuotedInteger(raw []byte) bool {
	for i := bytes.IndexByte(raw, '"'); i >= 0 && i+1 < len(raw); {
		c := raw[i+1]
		if c == '-' || (c >= '0' && c <= '9') {
			return true
		}
		j := bytes.IndexByte(raw[i+1:], '"')
		if j < 0 {
			return false
		}
		i += 1 + j
	}
	return false
}

// coerceEnumsIn mutates node (a DecodeTree object) in place and reports
// whether it changed anything.
func coerceEnumsIn(node any, desc protoreflect.MessageDescriptor) bool {
	obj, ok := node.(map[string]any)
	if !ok || desc == nil || isWKT(desc) {
		return false
	}
	changed := false
	for k, v := range obj {
		fd := fieldByWireKey(desc, k, nil)
		if fd == nil {
			continue
		}
		switch {
		case fd.IsMap():
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for mk, mv := range m {
				if nv, c := coerceEnumValue(fd.MapValue(), mv); c {
					m[mk] = nv
					changed = true
				}
			}
		case fd.IsList():
			l, ok := v.([]any)
			if !ok {
				continue
			}
			for i, e := range l {
				if nv, c := coerceEnumValue(fd, e); c {
					l[i] = nv
					changed = true
				}
			}
		default:
			if nv, c := coerceEnumValue(fd, v); c {
				obj[k] = nv
				changed = true
			}
		}
	}
	return changed
}

// coerceEnumValue handles one value of field fd (a list element or map
// value is passed with its element/value descriptor).
func coerceEnumValue(fd protoreflect.FieldDescriptor, v any) (any, bool) {
	switch fd.Kind() {
	case protoreflect.EnumKind:
		s, ok := v.(string)
		if !ok || fd.Enum().Values().ByName(protoreflect.Name(s)) != nil {
			return v, false
		}
		if _, err := strconv.ParseInt(s, 10, 32); err != nil {
			return v, false
		}
		return json.Number(s), true
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return v, coerceEnumsIn(v, fd.Message())
	}
	return v, false
}
