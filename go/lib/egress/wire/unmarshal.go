package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Issue is something the upstream sent that the client could not take as
// declared but that did not stop the decode: an enum string the client does
// not know (decoded as the _UNSPECIFIED zero value), a union tag it does not
// know (the union left unset). An upstream adding a value must not break the
// client's reads, and it must not pass unseen either; the runtime records
// issues on the call's span.
type Issue struct {
	Path    string
	Message string
}

// UnmarshalOptions configure Unmarshal.
type UnmarshalOptions struct {
	// Report receives every Issue. Nil drops them.
	Report func(Issue)
}

// Unmarshal reads the upstream's JSON into m. See UnmarshalOptions.Unmarshal.
func Unmarshal(data []byte, m proto.Message) error {
	return UnmarshalOptions{}.Unmarshal(data, m)
}

// Unmarshal reads the upstream's JSON into m.
//
// Three rules hold throughout: an unknown key is ignored (an upstream adding
// a field must never break the client), a JSON null leaves the field unset,
// and a value of the wrong JSON type is an error naming its place — the
// upstream broke the contract its own document states.
func (o UnmarshalOptions) Unmarshal(data []byte, m proto.Message) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the upstream's JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("the upstream's JSON: trailing data after the value")
	}
	// Replace, not merge — as protojson does: a reused message must not
	// carry the previous response's lists into this one.
	proto.Reset(m)
	return o.message(m.ProtoReflect(), v, "")
}

func (o UnmarshalOptions) report(path, format string, args ...any) {
	if o.Report != nil {
		if path == "" {
			path = "/"
		}
		o.Report(Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
}

func (o UnmarshalOptions) message(msg protoreflect.Message, v any, path string) error {
	if v == nil {
		return nil
	}
	md := msg.Descriptor()
	if isWKT(md) {
		raw, err := json.Marshal(v)
		if err != nil {
			return pathError(path, "%v", err)
		}
		if err := protojson.Unmarshal(raw, msg.Interface()); err != nil {
			return pathError(path, "%s: %v", md.FullName(), err)
		}
		return nil
	}
	p := planFor(md)
	switch {
	case p.union != nil:
		arm, err := o.chooseArm(p.union, v, path)
		if err != nil || arm == nil {
			return err
		}
		return o.field(msg, arm, v, path)
	case p.body != nil:
		return o.field(msg, p.body, v, path)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return pathError(path, "expected an object for %s, got %s", md.FullName(), kindOf(v))
	}
	for _, k := range sortedKeys(obj) {
		fp, known := p.byName[k]
		if !known {
			continue
		}
		if err := o.field(msg, fp, obj[k], path+"/"+k); err != nil {
			return err
		}
	}
	return nil
}

func (o UnmarshalOptions) field(msg protoreflect.Message, fp *fieldPlan, v any, path string) error {
	if v == nil {
		return nil
	}
	fd := fp.fd
	switch {
	case fd.IsList():
		return o.list(msg, fd, v, path)
	case fd.IsMap():
		return o.mapField(msg, fd, v, path)
	}
	val, err := o.singular(func() protoreflect.Value { return msg.NewField(fd) }, fd, v, path)
	if err != nil {
		return err
	}
	msg.Set(fd, val)
	return nil
}

func (o UnmarshalOptions) list(msg protoreflect.Message, fd protoreflect.FieldDescriptor, v any, path string) error {
	arr, ok := v.([]any)
	if !ok {
		return pathError(path, "expected an array, got %s", kindOf(v))
	}
	l := msg.Mutable(fd).List()
	for i, item := range arr {
		ip := path + "/" + strconv.Itoa(i)
		if item == nil {
			if !holdsNull(fd) {
				return pathError(ip, "null in an array of %s", fd.Kind())
			}
			l.Append(nullValue(l.NewElement()))
			continue
		}
		val, err := o.singular(l.NewElement, fd, item, ip)
		if err != nil {
			return err
		}
		l.Append(val)
	}
	return nil
}

func (o UnmarshalOptions) mapField(msg protoreflect.Message, fd protoreflect.FieldDescriptor, v any, path string) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return pathError(path, "expected an object, got %s", kindOf(v))
	}
	mp := msg.Mutable(fd).Map()
	for _, k := range sortedKeys(obj) {
		key := protoreflect.ValueOfString(k).MapKey()
		if obj[k] == nil {
			if holdsNull(fd.MapValue()) {
				mp.Set(key, nullValue(mp.NewValue()))
			}
			continue
		}
		val, err := o.singular(mp.NewValue, fd.MapValue(), obj[k], path+"/"+k)
		if err != nil {
			return err
		}
		mp.Set(key, val)
	}
	return nil
}

// singular decodes one value of fd's kind; newMsg makes the empty message a
// message-kind value is decoded into.
func (o UnmarshalOptions) singular(newMsg func() protoreflect.Value, fd protoreflect.FieldDescriptor, v any, path string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if b, ok := v.(bool); ok {
			return protoreflect.ValueOfBool(b), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return numeric(fd.Kind(), v, path)
	case protoreflect.StringKind:
		if s, ok := v.(string); ok {
			return protoreflect.ValueOfString(s), nil
		}
	case protoreflect.BytesKind:
		if s, ok := v.(string); ok {
			b, err := decodeBase64(s)
			if err != nil {
				return protoreflect.Value{}, pathError(path, "bytes: %v", err)
			}
			return protoreflect.ValueOfBytes(b), nil
		}
	case protoreflect.EnumKind:
		if s, ok := v.(string); ok {
			n, known := enumFor(fd.Enum()).fromWire[s]
			if !known {
				o.report(path, "unknown %s value %q, decoded as the zero value", fd.Enum().Name(), s)
			}
			return protoreflect.ValueOfEnum(n), nil
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		val := newMsg()
		if err := o.message(val.Message(), v, path); err != nil {
			return protoreflect.Value{}, err
		}
		return val, nil
	}
	return protoreflect.Value{}, pathError(path, "expected %s, got %s", fd.Kind(), kindOf(v))
}

// numeric decodes a JSON number (or a string holding one) as a numeric kind.
func numeric(kind protoreflect.Kind, v any, path string) (protoreflect.Value, error) {
	switch kind {
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		bits := 64
		if kind == protoreflect.FloatKind {
			bits = 32
		}
		f, err := strconv.ParseFloat(numText(v), bits)
		if err != nil {
			return protoreflect.Value{}, pathError(path, "expected a number, got %s", kindOf(v))
		}
		if bits == 32 {
			return protoreflect.ValueOfFloat32(float32(f)), nil
		}
		return protoreflect.ValueOfFloat64(f), nil
	}
	signed, bits := true, 64
	switch kind {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		bits = 32
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		signed, bits = false, 32
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		signed = false
	}
	n, err := intOf(v, signed, bits)
	if err != nil {
		return protoreflect.Value{}, pathError(path, "%v", err)
	}
	switch {
	case signed && bits == 32:
		return protoreflect.ValueOfInt32(int32(n.Int64())), nil
	case signed:
		return protoreflect.ValueOfInt64(n.Int64()), nil
	case bits == 32:
		return protoreflect.ValueOfUint32(uint32(n.Uint64())), nil
	}
	return protoreflect.ValueOfUint64(n.Uint64()), nil
}

// holdsNull reports a google.protobuf.Value element: the one type that has a
// null of its own, so a null inside its array or map is a value, not a hole.
func holdsNull(fd protoreflect.FieldDescriptor) bool {
	return fd.Message() != nil && fd.Message().FullName() == "google.protobuf.Value"
}

// nullValue sets an empty google.protobuf.Value to null_value.
func nullValue(v protoreflect.Value) protoreflect.Value {
	m := v.Message()
	null := m.Descriptor().Fields().ByName("null_value")
	m.Set(null, protoreflect.ValueOfEnum(0))
	return v
}

// jsonNumber is the JSON number grammar. A quoted value is read as a number
// only when it is one — "1_000", "+5" and "NaN" are not.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// numText returns the text of a JSON number, or of a string holding one
// (upstreams that quote their 64-bit ids, for JavaScript's sake, are
// common), or "" when v is neither.
func numText(v any) string {
	var t string
	switch x := v.(type) {
	case json.Number:
		t = x.String()
	case string:
		t = strings.TrimSpace(x)
	}
	if !jsonNumber.MatchString(t) {
		return ""
	}
	return t
}

// maxNumberText bounds the text of a number read as an integer: 1024 bits of
// precision hold about 308 digits, and anything longer is not a 64-bit
// integer in any honest notation.
const maxNumberText = 300

// magnitude returns the number of digits before the decimal point a JSON
// number has once its exponent is applied (log10, roughly), and whether its
// mantissa is zero — read from the text, with no arithmetic on the value.
func magnitude(t string) (mag int, zero bool) {
	t = strings.TrimPrefix(t, "-")
	mant, exp, _ := strings.Cut(strings.ToLower(t), "e")
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return 0, true
	}
	e := 0
	if exp != "" {
		n, err := strconv.Atoi(exp)
		if err != nil {
			// An exponent past int: as large, or as small, as it gets.
			if strings.HasPrefix(exp, "-") {
				return -(1 << 30), false
			}
			return 1 << 30, false
		}
		e = n
	}
	// Position of the first significant digit relative to the point.
	lead := len(strings.TrimLeft(intPart, "0"))
	if lead == 0 {
		lead = -(len(frac) - len(strings.TrimLeft(frac, "0")))
	}
	return lead + e, false
}

// intOf reads an integer of the given signedness and width. `1.0` and
// `1e3` are integers in JSON's eyes and are taken — exactly: the text is
// read as an arbitrary-precision number, so `1234567890123456789.0` is not
// rounded through a float64 on the way.
func intOf(v any, signed bool, bits int) (*big.Int, error) {
	t := numText(v)
	if t == "" {
		return nil, fmt.Errorf("expected an integer, got %s", kindOf(v))
	}
	// Bound the work before any big arithmetic: an integer that fits 64
	// bits has at most 20 significant digits, so a long text or a large
	// exponent can be settled from the text alone — `1e100000000` must not
	// allocate a hundred-million-digit integer to be found out of range.
	if len(t) > maxNumberText {
		return nil, fmt.Errorf("a %d-character number is not a %d-bit integer", len(t), bits)
	}
	mag, zero := magnitude(t)
	switch {
	case zero:
		// Any zero — 0e99999999999999999999 included, which big cannot parse.
		return new(big.Int), nil
	case mag <= 0:
		// Under 1 and not zero: never an integer, however tiny (big would
		// round 1e-999999999 to 0 and call it one).
		return nil, fmt.Errorf("expected an integer, got %s", t)
	case mag > 21:
		return nil, fmt.Errorf("%s does not fit %d bits", t, bits)
	}
	f, _, err := big.ParseFloat(t, 10, 1024, big.ToNearestEven)
	if err != nil || !f.IsInt() {
		return nil, fmt.Errorf("expected an integer, got %s", t)
	}
	n, _ := f.Int(nil)
	lo, hi := new(big.Int), new(big.Int).Lsh(big.NewInt(1), uint(bits))
	if signed {
		hi.Rsh(hi, 1)
		lo.Neg(hi)
	}
	if n.Cmp(lo) < 0 || n.Cmp(hi) >= 0 {
		return nil, fmt.Errorf("%s does not fit %d bits", t, bits)
	}
	return n, nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}

// kindOf names a decoded JSON value's kind.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedMap ranges a proto map in key order, so output is deterministic.
func sortedMap(m protoreflect.Map, f func(protoreflect.MapKey, protoreflect.Value) bool) {
	var keys []protoreflect.MapKey
	m.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		if !f(k, m.Get(k)) {
			return
		}
	}
}
