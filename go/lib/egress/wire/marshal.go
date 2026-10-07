package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Marshal renders m as the upstream's JSON.
//
// A field is written when it carries a value: a field with presence (a
// message, a proto3 `optional` scalar) when it is set, a list or map when it
// is non-empty, a required one always — a required scalar has no presence,
// so its zero value is a value the upstream asked for.
func Marshal(m proto.Message) ([]byte, error) {
	var b bytes.Buffer
	if err := writeMessage(&b, m.ProtoReflect(), ""); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeMessage(b *bytes.Buffer, msg protoreflect.Message, path string) error {
	md := msg.Descriptor()
	if isWrapper(md) {
		// The scalar wrappers: their value, by this package's own rules —
		// protojson would quote an Int64Value.
		fd := md.Fields().ByName("value")
		return writeSingular(b, fd, msg.Get(fd), path)
	}
	if isWKT(md) {
		raw, err := protojson.Marshal(msg.Interface())
		if err != nil {
			return pathError(path, "%v", err)
		}
		b.Write(raw)
		return nil
	}
	p := planFor(md)
	switch {
	case p.union != nil:
		return writeUnion(b, msg, p.union, path)
	case p.body != nil:
		return writeFieldValue(b, msg, p.body, path)
	}
	return writeObject(b, msg, p, path, "", "")
}

// writeObject writes msg as a JSON object. A non-empty discKey is a union's
// discriminator and discVal the selected arm's tag: the arm decides the
// discriminator, so the tag is written whatever the message's own field for
// it holds — in that field's place, or first when the message has none.
func writeObject(b *bytes.Buffer, msg protoreflect.Message, p *msgPlan, path, discKey, discVal string) error {
	b.WriteByte('{')
	first := true
	if _, own := p.byName[discKey]; discKey != "" && !own {
		writeKey(b, discKey, &first)
		writeString(b, discVal)
	}
	for _, fp := range p.fields {
		if discKey != "" && fp.name == discKey {
			writeKey(b, fp.name, &first)
			writeString(b, discVal)
			continue
		}
		if !emits(msg, fp) {
			continue
		}
		writeKey(b, fp.name, &first)
		if err := writeFieldValue(b, msg, fp, path+"/"+fp.name); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// emits is the presence rule of Marshal.
func emits(msg protoreflect.Message, fp *fieldPlan) bool {
	fd := fp.fd
	switch {
	case fd.IsList():
		return msg.Get(fd).List().Len() > 0 || fp.required
	case fd.IsMap():
		return msg.Get(fd).Map().Len() > 0 || fp.required
	case fd.HasPresence():
		return msg.Has(fd)
	}
	return true
}

func writeKey(b *bytes.Buffer, k string, first *bool) {
	if !*first {
		b.WriteByte(',')
	}
	*first = false
	writeString(b, k)
	b.WriteByte(':')
}

func writeUnion(b *bytes.Buffer, msg protoreflect.Message, u *unionPlan, path string) error {
	set := msg.WhichOneof(u.od)
	if set == nil {
		b.WriteString("null")
		return nil
	}
	var arm *fieldPlan
	for _, fp := range u.arms {
		if fp.fd == set {
			arm = fp
		}
	}
	if u.discriminator != "" {
		sub, sp, err := discriminatedArm(msg, arm, u, path)
		if err != nil {
			return err
		}
		return writeObject(b, sub, sp, path, u.discriminator, arm.tag)
	}
	return writeFieldValue(b, msg, arm, path)
}

// discriminatedArm returns the set arm of a discriminated union as the
// object it must be: the discriminator is a property, so an arm that is not
// a plain object (a wrapper, a nested union, a well-known type) has nowhere
// to carry it. The converter refuses to generate one; this refuses to send
// one rather than send a value the upstream cannot tell apart.
func discriminatedArm(msg protoreflect.Message, arm *fieldPlan, u *unionPlan, path string) (protoreflect.Message, *msgPlan, error) {
	if arm.fd.Message() == nil || isWKT(arm.fd.Message()) {
		return nil, nil, pathError(path, "arm %s of a union discriminated by %q is not an object", arm.fd.Name(), u.discriminator)
	}
	sub := msg.Get(arm.fd).Message()
	sp := planFor(sub.Descriptor())
	if sp.union != nil || sp.body != nil {
		return nil, nil, pathError(path, "arm %s of a union discriminated by %q is not an object", arm.fd.Name(), u.discriminator)
	}
	return sub, sp, nil
}

func writeFieldValue(b *bytes.Buffer, msg protoreflect.Message, fp *fieldPlan, path string) error {
	fd := fp.fd
	v := msg.Get(fd)
	switch {
	case fd.IsList():
		l := v.List()
		b.WriteByte('[')
		for i := 0; i < l.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeSingular(b, fd, l.Get(i), path+"/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case fd.IsMap():
		first := true
		b.WriteByte('{')
		var err error
		sortedMap(v.Map(), func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			if !utf8.ValidString(k.String()) {
				err = pathError(path, "map key is not valid UTF-8")
				return false
			}
			writeKey(b, k.String(), &first)
			err = writeSingular(b, fd.MapValue(), mv, path+"/"+k.String())
			return err == nil
		})
		if err != nil {
			return err
		}
		b.WriteByte('}')
		return nil
	case fd.HasPresence() && !msg.Has(fd) && fd.Message() == nil:
		b.WriteString("null")
		return nil
	}
	return writeSingular(b, fd, v, path)
}

func writeSingular(b *bytes.Buffer, fd protoreflect.FieldDescriptor, v protoreflect.Value, path string) error {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// 64-bit integers are JSON numbers here: protojson's strings are the
		// proto3 JSON mapping's choice, not the upstream's.
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return pathError(path, "%v has no JSON form", f)
		}
		bits := 64
		if fd.Kind() == protoreflect.FloatKind {
			bits = 32
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, bits))
	case protoreflect.StringKind:
		if !utf8.ValidString(v.String()) {
			// JSON has no form for it; replacing it with U+FFFD would send
			// a different string than the caller set.
			return pathError(path, "string is not valid UTF-8")
		}
		writeString(b, v.String())
	case protoreflect.BytesKind:
		writeString(b, base64.StdEncoding.EncodeToString(v.Bytes()))
	case protoreflect.EnumKind:
		s, ok := enumFor(fd.Enum()).toWire[v.Enum()]
		if !ok {
			return pathError(path, "%s value %d has no upstream string (an _UNSPECIFIED value cannot be sent)", fd.Enum().FullName(), v.Enum())
		}
		writeString(b, s)
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return writeMessage(b, v.Message(), path)
	}
	return nil
}

// writeString writes s as a JSON string without HTML escaping: an upstream
// expects `<` and `&` as themselves.
func writeString(b *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	b.Write(bytes.TrimSuffix(tmp.Bytes(), []byte("\n")))
}
