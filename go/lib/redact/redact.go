// Package redact encodes a request or response for the request event stream
// and the telemetry sinks: the protobuf wire form of the message WITHOUT the
// fields whose value may never leave the request, cut to fit a per-record
// ceiling — written straight from the message, never from a copy of it.
//
// The compiler decides what is left out and emits it as a [Plan] per message
// type (the `(w17.field)` gdpr / sensitive labels and their automatic rules);
// this package walks the message by reflection and writes protowire. The
// output is a valid encoding of the same message type, so a consumer decodes
// it with the type it already has: a left-out field reads as unset.
//
// What is never written, whatever the plan says:
//   - unknown fields (a newer client's field the gateway cannot classify);
//   - `google.protobuf.Any`, `Struct`, `Value` and `ListValue` — their content
//     has no declared type, so nothing is known about its sensitivity;
//   - a nested message of a type the plan set does not know (deny by default;
//     the other well-known types are plain and written whole).
//
// The cut: when the encoding is longer than the ceiling, the largest
// variable-size fields are trimmed first — every string, bytes, repeated or
// map field is capped at one common size, the smallest that brings the record
// under the ceiling ("water-filling"). A string is cut on a UTF-8 boundary, a
// list or map keeps its first entries that fit. A field nested inside a list
// is never a unit of its own: the list is. [Set.Append] reports the paths it
// cut, so the envelope can say the record is partial.
package redact

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Plan is the compiler's verdict on one message type.
type Plan struct {
	// Skip lists the fields whose value is never written (`sensitive`).
	Skip []protowire.Number
}

// Set is every plan of a bundle, by message full name.
type Set struct {
	plans map[protoreflect.FullName]Plan
}

// NewSet takes the generated plans, keyed by the message's full proto name
// without a leading dot ("acme.tasks.v1.CreateTaskReq").
func NewSet(plans map[string]Plan) *Set {
	s := &Set{plans: make(map[protoreflect.FullName]Plan, len(plans))}
	for name, p := range plans {
		s.plans[protoreflect.FullName(strings.TrimPrefix(name, "."))] = p
	}
	return s
}

// ErrNoPlan is returned for a top-level message the set has no plan for.
var ErrNoPlan = errors.New("redact: no plan for message type")

// ErrTooLarge is returned when not even emptying every variable-size field
// brings the record under the ceiling.
var ErrTooLarge = errors.New("redact: record does not fit the ceiling")

// Append appends m's redacted encoding to dst. A ceiling > 0 bounds the
// appended length; when the record had to be cut to fit, cut lists the cut
// fields' paths (dotted proto names, "items" or "profile.bio"). On an error
// dst is returned unchanged.
func (s *Set) Append(dst []byte, m proto.Message, ceiling int) (out []byte, cut []string, err error) {
	mr := m.ProtoReflect()
	if _, ok := s.plans[mr.Descriptor().FullName()]; !ok {
		return dst, nil, fmt.Errorf("%w %s", ErrNoPlan, mr.Descriptor().FullName())
	}
	e := encoders.Get().(*encoder)
	defer e.release()
	e.set = s

	base := len(dst)
	e.measure = ceiling > 0
	out = e.message(dst, mr, 0)
	if ceiling <= 0 || len(out)-base <= ceiling {
		return out, nil, nil
	}

	cap_, ok := waterLevel(e.units, len(out)-base-ceiling)
	if !ok {
		return dst, nil, ErrTooLarge
	}
	e.measure, e.units = false, e.units[:0]
	e.limit, e.cutting = cap_, true
	out = e.message(out[:base], mr, 0)
	if len(out)-base > ceiling {
		return dst, nil, ErrTooLarge
	}
	return out, e.cut, nil
}

// waterLevel finds the largest common cap that removes at least excess bytes
// from the units (each unit's encoded size), or false when even a cap of 0
// does not.
func waterLevel(units []int, excess int) (int, bool) {
	total := 0
	for _, u := range units {
		total += u
	}
	if total < excess {
		return 0, false
	}
	sorted := append([]int(nil), units...)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))
	// Lower the level unit by unit: with the k largest capped at level L,
	// the removed bytes are sum(sorted[:k]) - k*L.
	sum := 0
	for k := 1; k <= len(sorted); k++ {
		sum += sorted[k-1]
		next := 0
		if k < len(sorted) {
			next = sorted[k]
		}
		if sum-k*next >= excess {
			// The level lies in [next, sorted[k-1]]: the largest L with
			// sum - k*L >= excess.
			return (sum - excess) / k, true
		}
	}
	return 0, true
}

var encoders = sync.Pool{New: func() any { return &encoder{} }}

type encoder struct {
	set *Set

	// measure: record each unit's encoded size (first pass).
	measure bool
	units   []int

	// cutting: cap each unit at limit (second pass).
	cutting bool
	limit   int
	path    []protoreflect.Name
	cut     []string
}

func (e *encoder) release() {
	e.set, e.measure, e.cutting, e.limit = nil, false, false, 0
	e.units, e.path, e.cut = e.units[:0], e.path[:0], nil
	encoders.Put(e)
}

// inList > 0 while encoding inside a list or map: the enclosing field is the
// unit, so nothing below it is measured or cut on its own.
func (e *encoder) message(dst []byte, m protoreflect.Message, inList int) []byte {
	md := m.Descriptor()
	plan := e.set.plans[md.FullName()]
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) || skipped(plan.Skip, fd.Number()) || denied(fd) {
			continue
		}
		v := m.Get(fd)
		switch {
		case fd.IsMap() || fd.IsList():
			dst = e.unit(dst, fd, v, inList, e.collection)
		case fd.Kind() == protoreflect.StringKind || fd.Kind() == protoreflect.BytesKind:
			dst = e.unit(dst, fd, v, inList, e.text)
		default:
			dst = e.value(dst, fd, fd.Number(), v, inList)
		}
	}
	return dst
}

// unit encodes one variable-size field: measured in the first pass, capped
// in the second. Nested in a list it is plain.
func (e *encoder) unit(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value, inList int,
	enc func([]byte, protoreflect.FieldDescriptor, protoreflect.Value, int, int) []byte) []byte {
	if inList > 0 || (!e.measure && !e.cutting) {
		return enc(dst, fd, v, inList, math.MaxInt)
	}
	start := len(dst)
	if e.measure {
		dst = enc(dst, fd, v, inList, math.MaxInt)
		e.units = append(e.units, len(dst)-start)
		return dst
	}
	e.path = append(e.path, fd.Name())
	dst = enc(dst, fd, v, inList, e.limit)
	e.path = e.path[:len(e.path)-1]
	return dst
}

func (e *encoder) noteCut() {
	parts := make([]string, 0, len(e.path))
	for _, n := range e.path {
		parts = append(parts, string(n))
	}
	e.cut = append(e.cut, strings.Join(parts, "."))
}

// text writes a string or bytes field in at most limit bytes (tag and length
// included).
func (e *encoder) text(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value, _ int, limit int) []byte {
	var b []byte
	var s string
	isString := fd.Kind() == protoreflect.StringKind
	n := 0
	if isString {
		s = v.String()
		n = len(s)
	} else {
		b = v.Bytes()
		n = len(b)
	}
	tag := protowire.SizeTag(fd.Number())
	if tag+protowire.SizeBytes(n) > limit {
		room := limit - tag - protowire.SizeVarint(uint64(limit))
		if room < 0 {
			room = 0
		}
		if isString {
			for room > 0 && !utf8.RuneStart(s[room]) {
				room--
			}
			s = s[:room]
		} else {
			b = b[:room]
		}
		e.noteCut()
		if room == 0 {
			return dst
		}
	}
	dst = protowire.AppendTag(dst, fd.Number(), protowire.BytesType)
	if isString {
		return protowire.AppendString(dst, s)
	}
	return protowire.AppendBytes(dst, b)
}

// collection writes a repeated or map field, keeping the entries that fit in
// limit bytes.
func (e *encoder) collection(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value, inList int, limit int) []byte {
	if fd.IsMap() {
		return e.mapField(dst, fd, v.Map(), inList, limit)
	}
	start := len(dst)
	l := v.List()
	if fd.IsPacked() {
		dst = protowire.AppendTag(dst, fd.Number(), protowire.BytesType)
		body := len(dst)
		dst = append(dst, 0)
		for i := 0; i < l.Len(); i++ {
			mark := len(dst)
			dst = appendScalar(dst, fd, l.Get(i))
			if len(dst)-start+protowire.SizeVarint(uint64(len(dst)-body)) > limit {
				dst = dst[:mark]
				e.noteCut()
				break
			}
		}
		if len(dst) == body+1 {
			return dst[:start]
		}
		return fixLen(dst, body)
	}
	for i := 0; i < l.Len(); i++ {
		mark := len(dst)
		dst = e.value(dst, fd, fd.Number(), l.Get(i), inList+1)
		if len(dst)-start > limit {
			dst = dst[:mark]
			e.noteCut()
			break
		}
	}
	return dst
}

// mapField writes a map's entries that fit in limit bytes. (Range takes a
// closure, the one allocation a record with a map pays.)
func (e *encoder) mapField(dst []byte, fd protoreflect.FieldDescriptor, mp protoreflect.Map, inList int, limit int) []byte {
	start := len(dst)
	mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
		mark := len(dst)
		dst = protowire.AppendTag(dst, fd.Number(), protowire.BytesType)
		body := len(dst)
		dst = append(dst, 0)
		dst = e.value(dst, fd.MapKey(), 1, k.Value(), inList+1)
		dst = e.value(dst, fd.MapValue(), 2, mv, inList+1)
		dst = fixLen(dst, body)
		if len(dst)-start > limit {
			dst = dst[:mark]
			e.noteCut()
			return false
		}
		return true
	})
	return dst
}

// value writes one non-collection value under field number num.
func (e *encoder) value(dst []byte, fd protoreflect.FieldDescriptor, num protowire.Number, v protoreflect.Value, inList int) []byte {
	switch fd.Kind() {
	case protoreflect.StringKind:
		dst = protowire.AppendTag(dst, num, protowire.BytesType)
		return protowire.AppendString(dst, v.String())
	case protoreflect.BytesKind:
		dst = protowire.AppendTag(dst, num, protowire.BytesType)
		return protowire.AppendBytes(dst, v.Bytes())
	case protoreflect.MessageKind:
		if !e.writable(fd.Message().FullName()) {
			return dst
		}
		dst = protowire.AppendTag(dst, num, protowire.BytesType)
		body := len(dst)
		dst = append(dst, 0)
		if !e.cutting || inList > 0 {
			dst = e.message(dst, v.Message(), inList)
		} else {
			e.path = append(e.path, fd.Name())
			dst = e.message(dst, v.Message(), inList)
			e.path = e.path[:len(e.path)-1]
		}
		return fixLen(dst, body)
	case protoreflect.GroupKind:
		if !e.writable(fd.Message().FullName()) {
			return dst
		}
		dst = protowire.AppendTag(dst, num, protowire.StartGroupType)
		dst = e.message(dst, v.Message(), inList)
		return protowire.AppendTag(dst, num, protowire.EndGroupType)
	}
	dst = protowire.AppendTag(dst, num, wireType(fd.Kind()))
	return appendScalar(dst, fd, v)
}

// writable: a nested message is written when the set has its plan or it is
// a plain well-known type; any other type is left out whole.
func (e *encoder) writable(n protoreflect.FullName) bool {
	if _, ok := e.set.plans[n]; ok {
		return true
	}
	return plainWKT(n)
}

func wireType(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	}
	return protowire.VarintType
}

// appendScalar writes a numeric value without its tag.
func appendScalar(dst []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value) []byte {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protowire.AppendVarint(dst, protowire.EncodeBool(v.Bool()))
	case protoreflect.EnumKind:
		return protowire.AppendVarint(dst, uint64(v.Enum()))
	case protoreflect.Int32Kind, protoreflect.Int64Kind:
		return protowire.AppendVarint(dst, uint64(v.Int()))
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.AppendVarint(dst, v.Uint())
	case protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		return protowire.AppendVarint(dst, protowire.EncodeZigZag(v.Int()))
	case protoreflect.Fixed32Kind:
		return protowire.AppendFixed32(dst, uint32(v.Uint()))
	case protoreflect.Sfixed32Kind:
		return protowire.AppendFixed32(dst, uint32(v.Int()))
	case protoreflect.Fixed64Kind:
		return protowire.AppendFixed64(dst, v.Uint())
	case protoreflect.Sfixed64Kind:
		return protowire.AppendFixed64(dst, uint64(v.Int()))
	case protoreflect.FloatKind:
		return protowire.AppendFixed32(dst, math.Float32bits(float32(v.Float())))
	case protoreflect.DoubleKind:
		return protowire.AppendFixed64(dst, math.Float64bits(v.Float()))
	}
	return dst
}

// fixLen turns the one placeholder byte at start into the varint length of
// what follows it, shifting the body when the length needs more bytes.
func fixLen(dst []byte, start int) []byte {
	n := len(dst) - start - 1
	w := protowire.SizeVarint(uint64(n))
	if w > 1 {
		dst = append(dst, make([]byte, w-1)...)
		copy(dst[start+w:], dst[start+1:start+1+n])
	}
	protowire.AppendVarint(dst[start:start], uint64(n))
	return dst
}

func skipped(skip []protowire.Number, n protowire.Number) bool {
	for _, s := range skip {
		if s == n {
			return true
		}
	}
	return false
}

// denied: a message-typed field (or a list or map of them) whose content has
// no declared type.
func denied(fd protoreflect.FieldDescriptor) bool {
	if fd.IsMap() {
		fd = fd.MapValue()
	}
	return fd.Message() != nil && deniedMessage(fd.Message().FullName())
}

func deniedMessage(n protoreflect.FullName) bool {
	switch n {
	case "google.protobuf.Any", "google.protobuf.Struct", "google.protobuf.Value", "google.protobuf.ListValue":
		return true
	}
	return false
}

// plainWKT: the well-known types other than the denied ones carry no
// classifiable content (timestamps, durations, wrappers, field masks).
func plainWKT(n protoreflect.FullName) bool {
	return n.Parent() == "google.protobuf" && !deniedMessage(n)
}
