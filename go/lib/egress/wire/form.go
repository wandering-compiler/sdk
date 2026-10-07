package wire

import (
	"encoding/base64"
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Form renders m as an application/x-www-form-urlencoded body, nested
// values in the deep-object convention: `metadata[order_id]=42`,
// `items[0][price]=price_1`. It is how Stripe-style APIs take a request
// body, and — through AppendForm — how a `style: deepObject` query
// parameter is written. A form body is a set of named values, so m must
// render as an object: a wrapper around a scalar or an array has no form.
func Form(m proto.Message) (url.Values, error) {
	vals := url.Values{}
	if err := AppendForm(vals, "", m.ProtoReflect()); err != nil {
		return nil, err
	}
	return vals, nil
}

// AppendForm adds msg to vals under prefix ("" = at the top level).
func AppendForm(vals url.Values, prefix string, msg protoreflect.Message) error {
	md := msg.Descriptor()
	switch {
	case md.FullName() == "google.protobuf.Timestamp":
		// Read through reflection: a run-time descriptor's message is not
		// a *timestamppb.Timestamp.
		f := md.Fields()
		t := time.Unix(msg.Get(f.ByName("seconds")).Int(), msg.Get(f.ByName("nanos")).Int()).UTC()
		return addForm(vals, prefix, t.Format(time.RFC3339Nano))
	case isWrapper(md):
		return appendFormValue(vals, prefix, md.Fields().ByName("value"), msg.Get(md.Fields().ByName("value")))
	case isWKT(md):
		return appendFormWKT(vals, prefix, msg)
	}
	p := planFor(md)
	switch {
	case p.union != nil:
		return appendFormUnion(vals, prefix, msg, p.union)
	case p.body != nil:
		return appendFormField(vals, prefix, msg, p.body)
	}
	return appendFormFields(vals, prefix, msg, p, "")
}

func appendFormUnion(vals url.Values, prefix string, msg protoreflect.Message, u *unionPlan) error {
	set := msg.WhichOneof(u.od)
	if set == nil {
		return nil
	}
	for _, arm := range u.arms {
		if arm.fd != set {
			continue
		}
		if u.discriminator != "" {
			sub, sp, err := discriminatedArm(msg, arm, u, prefix)
			if err != nil {
				return err
			}
			// The arm decides the discriminator, as in Marshal.
			key, err := formKey(prefix, u.discriminator)
			if err != nil {
				return err
			}
			if err := addForm(vals, key, arm.tag); err != nil {
				return err
			}
			return appendFormFields(vals, prefix, sub, sp, u.discriminator)
		}
		return appendFormField(vals, prefix, msg, arm)
	}
	return nil
}

// appendFormWKT writes a free-form value — Struct, Value, ListValue — in the
// same deep-object shape as a message: objects by key, lists by index,
// scalars as their text; null is left out, as an unset field is.
func appendFormWKT(vals url.Values, prefix string, msg protoreflect.Message) error {
	switch msg.Descriptor().FullName() {
	case "google.protobuf.Struct":
		fields := msg.Get(msg.Descriptor().Fields().ByName("fields")).Map()
		var err error
		sortedMap(fields, func(k protoreflect.MapKey, v protoreflect.Value) bool {
			var key string
			if key, err = formKey(prefix, k.String()); err == nil {
				err = appendFormWKT(vals, key, v.Message())
			}
			return err == nil
		})
		return err
	case "google.protobuf.ListValue":
		l := msg.Get(msg.Descriptor().Fields().ByName("values")).List()
		for i := 0; i < l.Len(); i++ {
			if err := appendFormWKT(vals, prefix+"["+strconv.Itoa(i)+"]", l.Get(i).Message()); err != nil {
				return err
			}
		}
		return nil
	case "google.protobuf.Value":
		set := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("kind"))
		if set == nil || set.Name() == "null_value" {
			return nil
		}
		v := msg.Get(set)
		switch set.Name() {
		case "number_value":
			if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
				return pathError(prefix, "%v has no form encoding", f)
			}
			return addForm(vals, prefix, formFloat(v.Float(), 64))
		case "string_value":
			return addForm(vals, prefix, v.String())
		case "bool_value":
			return addForm(vals, prefix, strconv.FormatBool(v.Bool()))
		}
		return appendFormWKT(vals, prefix, v.Message())
	}
	return pathError(prefix, "%s has no form encoding", msg.Descriptor().FullName())
}

// addForm adds one value. A deep-object key has no escape for `[` and `]`,
// and a value needs a name: either would be read back as something else.
func addForm(vals url.Values, key, value string) error {
	if key == "" {
		return errors.New("a form body must be an object: this value has no name to go under")
	}
	vals.Add(key, value)
	return nil
}

// appendFormFields adds an object's fields, skipping the one named skip.
func appendFormFields(vals url.Values, prefix string, msg protoreflect.Message, p *msgPlan, skip string) error {
	for _, fp := range p.fields {
		if fp.name == skip || !emits(msg, fp) {
			continue
		}
		key, err := formKey(prefix, fp.name)
		if err != nil {
			return err
		}
		if err := appendFormField(vals, key, msg, fp); err != nil {
			return err
		}
	}
	return nil
}

// formKey nests name under prefix. A deep-object key has no escape: a name
// holding `[` or `]` would be read back as a different path, and an empty
// one as an array append (`m[]=v`) — both are refused, here, before any
// value is written under them.
func formKey(prefix, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "[]") {
		return "", pathError(prefix, "form key %q cannot be written unambiguously", name)
	}
	if prefix == "" {
		return name, nil
	}
	return prefix + "[" + name + "]", nil
}

func appendFormField(vals url.Values, key string, msg protoreflect.Message, fp *fieldPlan) error {
	fd := fp.fd
	v := msg.Get(fd)
	switch {
	case fd.IsList():
		if key == "" {
			return addForm(vals, "", "")
		}
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			if err := appendFormValue(vals, key+"["+strconv.Itoa(i)+"]", fd, l.Get(i)); err != nil {
				return err
			}
		}
		return nil
	case fd.IsMap():
		var err error
		sortedMap(v.Map(), func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			var sub string
			if sub, err = formKey(key, k.String()); err == nil {
				err = appendFormValue(vals, sub, fd.MapValue(), mv)
			}
			return err == nil
		})
		return err
	}
	return appendFormValue(vals, key, fd, v)
}

func appendFormValue(vals url.Values, key string, fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return AppendForm(vals, key, v.Message())
	case protoreflect.BoolKind:
		return addForm(vals, key, strconv.FormatBool(v.Bool()))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return addForm(vals, key, strconv.FormatInt(v.Int(), 10))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return addForm(vals, key, strconv.FormatUint(v.Uint(), 10))
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return pathError(key, "%v has no form encoding", f)
		}
		bits := 64
		if fd.Kind() == protoreflect.FloatKind {
			bits = 32
		}
		return addForm(vals, key, formFloat(f, bits))
	case protoreflect.StringKind:
		return addForm(vals, key, v.String())
	case protoreflect.BytesKind:
		return addForm(vals, key, base64.StdEncoding.EncodeToString(v.Bytes()))
	case protoreflect.EnumKind:
		s, ok := enumFor(fd.Enum()).toWire[v.Enum()]
		if !ok {
			return pathError(key, "%s value %d has no upstream string", fd.Enum().FullName(), v.Enum())
		}
		return addForm(vals, key, s)
	}
	return nil
}

// formFloat is a float's shortest text, in exponent form only when the
// plain form would be long (1e300 is not 301 digits on the wire).
func formFloat(f float64, bits int) string {
	return strconv.FormatFloat(f, 'g', -1, bits)
}
