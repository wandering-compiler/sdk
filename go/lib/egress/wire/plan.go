// Package wire is the JSON (and form) codec of a generated egress client:
// it renders a proto message the way the third-party API writes it, and
// reads the API's JSON back.
//
// Everything it knows about the upstream comes from the options the
// compiler put on the client's proto (proto/w17/egress.proto): the exact
// JSON key of each field, the string each enum value travels as, the
// discriminator of each union, which message is only a wrapper around a
// JSON array or scalar. It decides nothing itself — a shape the options
// cannot describe is the converter's to refuse, not this package's to
// guess.
//
// It is a direct walk over the message descriptor rather than a rewrite of
// protojson's output: the w17 dialect differs from protojson in two places
// and post-processes them (protojsonx), but an upstream differs in nearly
// every one — keys, enums, 64-bit integers, union shape, wrappers — and
// undoing all of protojson's choices would be a codec of its own anyway.
// protojson is still used where it is right: the well-known types
// (Timestamp, Struct, Value, …), whose JSON forms are the standard ones.
package wire

import (
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// fieldPlan is one field's wire identity.
type fieldPlan struct {
	fd       protoreflect.FieldDescriptor
	name     string // the JSON key / parameter name
	in       w17pb.EgressIn
	required bool
	tag      string // union arm: the discriminator value
}

// unionPlan is a oneof marked (w17.egress_union).
type unionPlan struct {
	od            protoreflect.OneofDescriptor
	discriminator string
	arms          []*fieldPlan
	byTag         map[string]*fieldPlan
}

// msgPlan is how a message travels.
type msgPlan struct {
	fields []*fieldPlan
	byName map[string]*fieldPlan
	// union: the message IS the union — on the wire, its set arm's value.
	union *unionPlan
	// body: the message is a wrapper — on the wire, this field's value.
	body *fieldPlan
}

// plans and enums cache per descriptor, without eviction: the descriptors
// are the program's linked, generated ones, a fixed set. A caller that built
// descriptors at run time on every call would grow them; nothing does.
var plans sync.Map // protoreflect.MessageDescriptor → *msgPlan

func planFor(md protoreflect.MessageDescriptor) *msgPlan {
	if p, ok := plans.Load(md); ok {
		return p.(*msgPlan)
	}
	p := buildPlan(md)
	actual, _ := plans.LoadOrStore(md, p)
	return actual.(*msgPlan)
}

func buildPlan(md protoreflect.MessageDescriptor) *msgPlan {
	p := &msgPlan{byName: map[string]*fieldPlan{}}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		opt := fieldOptions(fd)
		fp := &fieldPlan{fd: fd, name: opt.GetName(), in: opt.GetIn(), required: opt.GetRequired(), tag: opt.GetTag()}
		if fp.name == "" {
			fp.name = string(fd.Name())
		}
		p.fields = append(p.fields, fp)
		if _, dup := p.byName[fp.name]; !dup {
			p.byName[fp.name] = fp
		}
	}
	oneofs := md.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		if od.IsSynthetic() {
			continue
		}
		u := typedOption(od.Options(), w17pb.E_EgressUnion)
		if u == nil {
			continue
		}
		up := &unionPlan{od: od, discriminator: u.(*w17pb.EgressUnion).GetDiscriminator(), byTag: map[string]*fieldPlan{}}
		for _, fp := range p.fields {
			if fp.fd.ContainingOneof() == od {
				up.arms = append(up.arms, fp)
				if fp.tag != "" {
					up.byTag[fp.tag] = fp
				}
			}
		}
		p.union = up
	}
	if p.union == nil && len(p.fields) == 1 && p.fields[0].in == w17pb.EgressIn_EGRESS_IN_BODY {
		p.body = p.fields[0]
	}
	return p
}

func fieldOptions(fd protoreflect.FieldDescriptor) *w17pb.EgressField {
	if v := typedOption(fd.Options(), w17pb.E_EgressField); v != nil {
		return v.(*w17pb.EgressField)
	}
	return nil
}

// enumWire maps an enum's values to and from the upstream's strings.
type enumWire struct {
	toWire   map[protoreflect.EnumNumber]string
	fromWire map[string]protoreflect.EnumNumber
}

var enums sync.Map // protoreflect.EnumDescriptor → *enumWire

func enumFor(ed protoreflect.EnumDescriptor) *enumWire {
	if e, ok := enums.Load(ed); ok {
		return e.(*enumWire)
	}
	e := &enumWire{toWire: map[protoreflect.EnumNumber]string{}, fromWire: map[string]protoreflect.EnumNumber{}}
	values := ed.Values()
	for i := 0; i < values.Len(); i++ {
		v := values.Get(i)
		up := typedOption(v.Options(), w17pb.E_EgressEnumValue)
		if up == nil {
			continue // the _UNSPECIFIED zero value carries none
		}
		s := up.(string)
		e.toWire[v.Number()] = s
		if _, dup := e.fromWire[s]; !dup {
			e.fromWire[s] = v.Number()
		}
	}
	actual, _ := enums.LoadOrStore(ed, e)
	return actual.(*enumWire)
}

// typedOption reads an extension off a descriptor's options, or nil when it
// is not set. A Go-generated descriptor's extension values are the Go
// types; a descriptor built at run time (protocompile, dynamicpb) holds them
// as dynamic messages, which GetExtension cannot hand back as the Go type.
// So the options are always re-decoded against the linked types first —
// once per descriptor, since plans are cached.
func typedOption(opts proto.Message, xt protoreflect.ExtensionType) any {
	if opts == nil || !opts.ProtoReflect().IsValid() {
		return nil
	}
	typed := retype(opts)
	if typed == nil || !proto.HasExtension(typed, xt) {
		return nil
	}
	return proto.GetExtension(typed, xt)
}

func retype(opts proto.Message) proto.Message {
	var typed proto.Message
	switch opts.ProtoReflect().Descriptor().FullName() {
	case "google.protobuf.FieldOptions":
		typed = &descriptorpb.FieldOptions{}
	case "google.protobuf.OneofOptions":
		typed = &descriptorpb.OneofOptions{}
	case "google.protobuf.EnumValueOptions":
		typed = &descriptorpb.EnumValueOptions{}
	default:
		return nil
	}
	b, err := proto.Marshal(opts)
	if err != nil {
		return nil
	}
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(b, typed); err != nil {
		return nil
	}
	return typed
}

// isWKT reports a google.protobuf message, whose JSON form protojson owns.
func isWKT(md protoreflect.MessageDescriptor) bool {
	return strings.HasPrefix(string(md.FullName()), "google.protobuf.")
}

// isWrapper reports the scalar wrapper types (google.protobuf.Int64Value, …).
func isWrapper(md protoreflect.MessageDescriptor) bool {
	return isWKT(md) && strings.HasSuffix(string(md.Name()), "Value") &&
		md.Name() != "Value" && md.Name() != "ListValue" && md.Fields().ByName("value") != nil
}

// pathError is an error at a place in the JSON.
func pathError(path, format string, args ...any) error {
	if path == "" {
		path = "/"
	}
	return fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
}
