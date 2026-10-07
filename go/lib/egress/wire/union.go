package wire

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// chooseArm picks the arm of a union a JSON value is. With a discriminator
// it is the arm tagged with the value of that property; an unknown tag is
// an Issue and leaves the union unset (nil, nil). Without one it is chosen
// by shape — see byShape.
func (o UnmarshalOptions) chooseArm(u *unionPlan, v any, path string) (*fieldPlan, error) {
	if u.discriminator != "" {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, pathError(path, "expected an object carrying %q, got %s", u.discriminator, kindOf(v))
		}
		tag, _ := obj[u.discriminator].(string)
		if arm, known := u.byTag[tag]; known {
			return arm, nil
		}
		o.report(path, "unknown %s %q, left unset", u.discriminator, tag)
		return nil, nil
	}
	return byShape(u, v, path)
}

// byShape picks an untagged arm. A scalar or an array goes to the one arm
// that takes its JSON kind. An object goes to the object arm whose
// required properties are all present; when several qualify, to the one
// with the fewest properties foreign to it, then to the one that knows the
// most of them, then to the one declaring the fewest — of two arms that
// take the object equally, the smaller is the exact fit. An arm whose shape is open — a free-form Struct or Value, a nested
// union — is the fallback when no closed arm takes the value.
//
// The converter refuses unions whose arms cannot be told apart this way,
// so ambiguity here means the upstream sent something its document does not
// describe.
func byShape(u *unionPlan, v any, path string) (*fieldPlan, error) {
	kind := kindOf(v)
	var closed, open []*fieldPlan
	for _, arm := range u.arms {
		switch armTakes(arm.fd, kind) {
		case takesClosed:
			closed = append(closed, arm)
		case takesOpen:
			open = append(open, arm)
		}
	}
	if kind == "object" && len(closed) > 1 {
		closed = bestObjectArms(closed, v.(map[string]any))
	}
	switch {
	case len(closed) == 1:
		return closed[0], nil
	case len(closed) > 1:
		return nil, pathError(path, "the value fits several variants (%s) and the union has no discriminator", armNames(closed))
	case len(open) == 1:
		return open[0], nil
	case len(open) > 1:
		return nil, pathError(path, "the value fits several open variants (%s)", armNames(open))
	}
	return nil, pathError(path, "no variant of the union takes a JSON %s", kind)
}

type takes int

const (
	takesNot takes = iota
	takesClosed
	takesOpen
)

// armTakes reports whether an arm's type accepts a JSON kind, and whether
// it does so as a closed shape (a declared object, a scalar) or an open one.
func armTakes(fd protoreflect.FieldDescriptor, kind string) takes {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return when(kind == "boolean")
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.EnumKind:
		return when(kind == "string")
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return messageTakes(fd.Message(), kind)
	}
	return when(kind == "number") // every numeric kind
}

func when(ok bool) takes {
	if ok {
		return takesClosed
	}
	return takesNot
}

func messageTakes(md protoreflect.MessageDescriptor, kind string) takes {
	switch md.FullName() {
	case "google.protobuf.Struct":
		return openWhen(kind == "object")
	case "google.protobuf.Value":
		return takesOpen
	case "google.protobuf.ListValue":
		return openWhen(kind == "array")
	case "google.protobuf.Timestamp", "google.protobuf.Duration", "google.protobuf.FieldMask":
		return when(kind == "string")
	}
	if isWrapper(md) {
		return armTakes(md.Fields().ByName("value"), kind)
	}
	if isWKT(md) { // Empty, Any and the rest: an object, of a shape not declared here
		return openWhen(kind == "object")
	}
	p := planFor(md)
	switch {
	case p.union != nil:
		return takesOpen
	case p.body != nil:
		fd := p.body.fd
		switch {
		case fd.IsList():
			return when(kind == "array")
		case fd.IsMap():
			return when(kind == "object")
		}
		return armTakes(fd, kind)
	}
	return when(kind == "object")
}

func openWhen(ok bool) takes {
	if ok {
		return takesOpen
	}
	return takesNot
}

// armScore is how well an object arm fits a JSON object. declared breaks the
// last tie: of two arms that take the object equally, the one declaring
// fewer properties is the exact fit — the other merely allows more.
type armScore struct {
	arm                        *fieldPlan
	foreign, matched, declared int
}

// scoreArm scores arm against obj; ok is false when a required property
// of the arm is missing.
func scoreArm(arm *fieldPlan, obj map[string]any) (s armScore, ok bool) {
	p := planFor(arm.fd.Message())
	for _, fp := range p.fields {
		if _, present := obj[fp.name]; fp.required && !present {
			return armScore{}, false
		}
	}
	s.arm, s.declared = arm, len(p.fields)
	for k := range obj {
		if _, known := p.byName[k]; known {
			s.matched++
		} else {
			s.foreign++
		}
	}
	return s, true
}

func (s armScore) better(o armScore) bool {
	if s.foreign != o.foreign {
		return s.foreign < o.foreign
	}
	if s.matched != o.matched {
		return s.matched > o.matched
	}
	return s.declared < o.declared
}

// bestObjectArms narrows object arms by the rules in byShape.
func bestObjectArms(arms []*fieldPlan, obj map[string]any) []*fieldPlan {
	var best []armScore
	for _, arm := range arms {
		s, ok := scoreArm(arm, obj)
		switch {
		case !ok:
		case len(best) == 0 || s.better(best[0]):
			best = []armScore{s}
		case !best[0].better(s):
			best = append(best, s)
		}
	}
	out := make([]*fieldPlan, 0, len(best))
	for _, s := range best {
		out = append(out, s.arm)
	}
	return out
}

func armNames(arms []*fieldPlan) string {
	names := make([]string, 0, len(arms))
	for _, a := range arms {
		names = append(names, string(a.fd.Name()))
	}
	return strings.Join(names, ", ")
}
