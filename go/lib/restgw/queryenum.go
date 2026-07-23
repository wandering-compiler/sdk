package restgw

import (
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// SetEnumField assigns an enum-typed request field from its URL
// query-string form.
//
// Enum query params used to be unrepresentable: the gateway's decode
// generator skipped every enum field, and so did the OpenAPI and client
// emitters, so a body-less method (GET / SSE stream) simply dropped it.
// The field could not be recovered from a JSON body either, because such
// a method has no body — the filter silently did nothing.
//
// The value is resolved against the field's own descriptor, accepting
// either the enum VALUE NAME (the protojson form, e.g. "WIDGET_STATUS_
// ACTIVE") or its decimal number — the same pair protojson itself
// accepts, so a query param and a JSON body spell the value the same way.
//
// Errors name the field and list the accepted values, so a caller who
// guesses a spelling gets told the right one instead of a silent no-op.
func SetEnumField(msg proto.Message, field, raw string) error {
	if msg == nil {
		return fmt.Errorf("set enum %q: nil message", field)
	}
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil {
		return fmt.Errorf("set enum %q: no such field on %s", field, m.Descriptor().FullName())
	}
	if fd.Kind() != protoreflect.EnumKind {
		return fmt.Errorf("set enum %q: field is %s, not an enum", field, fd.Kind())
	}
	num, err := enumNumber(fd.Enum(), raw)
	if err != nil {
		return fmt.Errorf("query param %s: %w", field, err)
	}
	m.Set(fd, protoreflect.ValueOfEnum(num))
	return nil
}

// enumNumber resolves one enum value from its name or decimal number.
// A numeric form outside the declared set is REFUSED rather than passed
// through: proto3 tolerates unknown enum numbers on the wire for forward
// compatibility, but a URL the caller typed is not a forward-compat
// concern — it is a typo, and accepting it would hand the backend a
// value with no meaning.
func enumNumber(ed protoreflect.EnumDescriptor, raw string) (protoreflect.EnumNumber, error) {
	values := ed.Values()
	if v := values.ByName(protoreflect.Name(raw)); v != nil {
		return v.Number(), nil
	}
	if n, err := strconv.ParseInt(raw, 10, 32); err == nil {
		if v := values.ByNumber(protoreflect.EnumNumber(n)); v != nil {
			return v.Number(), nil
		}
	}
	return 0, fmt.Errorf("%q is not a value of %s (accepted: %s)",
		raw, ed.FullName(), enumValueList(ed))
}

// enumValueList renders the declared value names for an error message.
func enumValueList(ed protoreflect.EnumDescriptor) string {
	values := ed.Values()
	out := make([]byte, 0, values.Len()*16)
	for i := 0; i < values.Len(); i++ {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, values.Get(i).Name()...)
	}
	return string(out)
}
