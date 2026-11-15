package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// SOAP — a document/literal SOAP 1.1 / 1.2 call. The request message IS the
// content of the call's request element inside soap:Body, and the response
// element's content decodes into the response message. Every field carries
// its XML identity in (w17.egress_field): the local name, the namespace it
// is qualified with (empty: unqualified), and whether it is an attribute.

const (
	soap11Envelope = "http://schemas.xmlsoap.org/soap/envelope/"
	soap12Envelope = "http://www.w3.org/2003/05/soap-envelope"
)

// IsSOAP reports whether the call is a SOAP one.
func IsSOAP(call *w17pb.EgressCall) bool {
	switch call.GetRequestEncoding() {
	case w17pb.EgressEncoding_EGRESS_ENCODING_SOAP11, w17pb.EgressEncoding_EGRESS_ENCODING_SOAP12:
		return true
	}
	return false
}

// SOAPContentType is the request's content type: text/xml for SOAP 1.1,
// application/soap+xml with the action for SOAP 1.2.
func SOAPContentType(call *w17pb.EgressCall) string {
	if call.GetRequestEncoding() == w17pb.EgressEncoding_EGRESS_ENCODING_SOAP12 {
		ct := "application/soap+xml; charset=utf-8"
		if a := call.GetSoapAction(); a != "" {
			ct += `; action="` + a + `"`
		}
		return ct
	}
	return "text/xml; charset=utf-8"
}

// MarshalSOAP renders the envelope carrying msg as the call's request
// element.
func MarshalSOAP(call *w17pb.EgressCall, msg proto.Message) ([]byte, error) {
	envNS := soap11Envelope
	if call.GetRequestEncoding() == w17pb.EgressEncoding_EGRESS_ENCODING_SOAP12 {
		envNS = soap12Envelope
	}
	w := &xmlWriter{prefix: map[string]string{}}
	w.collect(call.GetXmlNamespace())
	w.collectMessage(msg.ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})
	w.buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	w.buf.WriteString(`<soapenv:Envelope xmlns:soapenv="` + envNS + `"`)
	for _, ns := range w.order {
		w.buf.WriteString(` xmlns:` + w.prefix[ns] + `="`)
		w.escape(ns)
		w.buf.WriteString(`"`)
	}
	w.buf.WriteString(`><soapenv:Header/><soapenv:Body>`)
	if err := w.element(call.GetXmlNamespace(), call.GetRequestElement(), msg.ProtoReflect()); err != nil {
		return nil, err
	}
	w.buf.WriteString(`</soapenv:Body></soapenv:Envelope>`)
	return w.buf.Bytes(), nil
}

type xmlWriter struct {
	buf    bytes.Buffer
	prefix map[string]string
	order  []string
}

// collect gives a namespace its prefix, declared once on the envelope: an
// unqualified element must not inherit a default namespace, so none is
// ever declared.
func (w *xmlWriter) collect(ns string) {
	if ns == "" || w.prefix[ns] != "" {
		return
	}
	w.prefix[ns] = "ns" + strconv.Itoa(len(w.order)+1)
	w.order = append(w.order, ns)
}

func (w *xmlWriter) collectMessage(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
	if seen[md.FullName()] {
		return
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if o := fieldOptions(fd); o != nil {
			w.collect(o.GetXmlNamespace())
		}
		if fd.Message() != nil && fd.Message().FullName() != timestampName {
			w.collectMessage(fd.Message(), seen)
		}
	}
}

func (w *xmlWriter) qname(ns, local string) string {
	if p := w.prefix[ns]; p != "" {
		return p + ":" + local
	}
	return local
}

func (w *xmlWriter) escape(s string) {
	_ = xml.EscapeText(&w.buf, []byte(s))
}

// element writes <name attrs>children</name> for a message value.
func (w *xmlWriter) element(ns, local string, m protoreflect.Message) error {
	name := w.qname(ns, local)
	w.buf.WriteString("<" + name)
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		o := fieldOptions(fd)
		if o == nil || !o.GetXmlAttribute() || !m.Has(fd) {
			continue
		}
		text, err := xmlText(fd, m.Get(fd))
		if err != nil {
			return err
		}
		w.buf.WriteString(" " + w.qname(o.GetXmlNamespace(), o.GetName()) + `="`)
		w.escape(text)
		w.buf.WriteString(`"`)
	}
	w.buf.WriteString(">")
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if o := fieldOptions(fd); o != nil && o.GetXmlText() && m.Has(fd) {
			text, err := xmlText(fd, m.Get(fd))
			if err != nil {
				return err
			}
			w.escape(text)
		}
	}
	// Children in field-number order: the generator numbers a sequence's
	// elements in schema order, and a sequence is ordered.
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		o := fieldOptions(fd)
		if o == nil || o.GetXmlAttribute() || o.GetXmlText() || !m.Has(fd) {
			continue
		}
		if err := w.field(o, fd, m.Get(fd)); err != nil {
			return err
		}
	}
	w.buf.WriteString("</" + name + ">")
	return nil
}

func (w *xmlWriter) field(o *w17pb.EgressField, fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	if fd.IsList() {
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			if err := w.value(o, fd, l.Get(i)); err != nil {
				return err
			}
		}
		return nil
	}
	return w.value(o, fd, v)
}

func (w *xmlWriter) value(o *w17pb.EgressField, fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	if fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() != timestampName {
		return w.element(o.GetXmlNamespace(), o.GetName(), v.Message())
	}
	text, err := xmlText(fd, v)
	if err != nil {
		return err
	}
	name := w.qname(o.GetXmlNamespace(), o.GetName())
	w.buf.WriteString("<" + name + ">")
	w.escape(text)
	w.buf.WriteString("</" + name + ">")
	return nil
}

const timestampName = "google.protobuf.Timestamp"

// scalarText is the XSD lexical form of a scalar value.
func xmlText(fd protoreflect.FieldDescriptor, v protoreflect.Value) (string, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return v.String(), nil
	case protoreflect.BoolKind:
		return strconv.FormatBool(v.Bool()), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(v.Int(), 10), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10), nil
	case protoreflect.FloatKind:
		return strconv.FormatFloat(v.Float(), 'f', -1, 32), nil
	case protoreflect.DoubleKind:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64), nil
	case protoreflect.BytesKind:
		return base64.StdEncoding.EncodeToString(v.Bytes()), nil
	case protoreflect.EnumKind:
		ev := fd.Enum().Values().ByNumber(v.Enum())
		if ev == nil {
			return "", fmt.Errorf("%s: enum value %d has no name", fd.FullName(), v.Enum())
		}
		if s := enumValueWire(ev); s != "" {
			return s, nil
		}
		return string(ev.Name()), nil
	case protoreflect.MessageKind:
		if fd.Message().FullName() == timestampName {
			ts := v.Message().Interface().(*timestamppb.Timestamp)
			return ts.AsTime().UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", fmt.Errorf("%s: no XML form for %s", fd.FullName(), fd.Kind())
}

func enumValueWire(ev protoreflect.EnumValueDescriptor) string {
	if v := typedOption(ev.Options(), w17pb.E_EgressEnumValue); v != nil {
		return v.(string)
	}
	return ""
}

// UnmarshalSOAP decodes a success envelope: the first child of soap:Body is
// the response element, whose content fills out. Elements and attributes
// match fields by local name; one the message does not name is skipped.
func UnmarshalSOAP(raw []byte, out proto.Message) error {
	d := xml.NewDecoder(bytes.NewReader(raw))
	if err := toBody(d); err != nil {
		return err
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("soap: no response element in Body: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return decodeInto(d, se, out.ProtoReflect())
		}
	}
}

// toBody advances d to just inside soap:Body.
func toBody(d *xml.Decoder) error {
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("soap: the response has no Body")
			}
			return fmt.Errorf("soap: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Body" &&
			(se.Name.Space == soap11Envelope || se.Name.Space == soap12Envelope) {
			return nil
		}
	}
}

const xsiNamespace = "http://www.w3.org/2001/XMLSchema-instance"

// isNil reports an element marked xsi:nil="true": no value, the field stays
// unset.
func isNil(se xml.StartElement) bool {
	for _, a := range se.Attr {
		if a.Name.Space == xsiNamespace && a.Name.Local == "nil" {
			return a.Value == "true" || a.Value == "1"
		}
	}
	return false
}

func decodeInto(d *xml.Decoder, start xml.StartElement, m protoreflect.Message) error {
	byElem, byAttr, textField := xmlFields(m.Descriptor())
	var text strings.Builder
	for _, a := range start.Attr {
		fd := byAttr[a.Name.Local]
		if fd == nil || (a.Value == "" && fd.Kind() != protoreflect.StringKind && fd.Kind() != protoreflect.BytesKind) {
			continue
		}
		v, err := parseScalar(fd, a.Value)
		if err != nil {
			return err
		}
		m.Set(fd, v)
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("soap: %s: %w", start.Name.Local, err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			if textField != nil {
				text.Write(t)
			}
		case xml.EndElement:
			if textField != nil {
				if s := strings.TrimSpace(text.String()); s != "" || textField.Kind() == protoreflect.StringKind {
					v, err := parseScalar(textField, s)
					if err != nil {
						return err
					}
					m.Set(textField, v)
				}
			}
			return nil
		case xml.StartElement:
			fd := byElem[t.Name.Local]
			if fd == nil || isNil(t) {
				if err := d.Skip(); err != nil {
					return err
				}
				continue
			}
			var v protoreflect.Value
			if fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() != timestampName {
				var sub protoreflect.Message
				if fd.IsList() {
					sub = m.Mutable(fd).List().NewElement().Message()
				} else {
					sub = m.Mutable(fd).Message()
				}
				if err := decodeInto(d, t, sub); err != nil {
					return err
				}
				v = protoreflect.ValueOfMessage(sub)
			} else {
				var raw string
				if err := d.DecodeElement(&raw, &t); err != nil {
					return err
				}
				raw = strings.TrimSpace(raw)
				if raw == "" && fd.Kind() != protoreflect.StringKind && fd.Kind() != protoreflect.BytesKind {
					// An empty number, date or enum is no value.
					continue
				}
				v, err = parseScalar(fd, raw)
				if err != nil {
					return err
				}
			}
			switch {
			case fd.IsList():
				m.Mutable(fd).List().Append(v)
			case fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() != timestampName:
				// Already filled in place through Mutable.
			default:
				m.Set(fd, v)
			}
		}
	}
}

func xmlFields(md protoreflect.MessageDescriptor) (elems, attrs map[string]protoreflect.FieldDescriptor, text protoreflect.FieldDescriptor) {
	elems, attrs = map[string]protoreflect.FieldDescriptor{}, map[string]protoreflect.FieldDescriptor{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		o := fieldOptions(fd)
		if o == nil {
			continue
		}
		switch {
		case o.GetXmlText():
			text = fd
		case o.GetXmlAttribute():
			attrs[o.GetName()] = fd
		default:
			elems[o.GetName()] = fd
		}
	}
	return elems, attrs, text
}

// parseScalar reads the XSD lexical form of a scalar.
func parseScalar(fd protoreflect.FieldDescriptor, s string) (protoreflect.Value, error) {
	bad := func(err error) (protoreflect.Value, error) {
		return protoreflect.Value{}, fmt.Errorf("soap: %s: %q: %w", fd.Name(), s, err)
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfUint64(n), nil
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.BytesKind:
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfBytes(b), nil
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		for i := 0; i < vals.Len(); i++ {
			ev := vals.Get(i)
			if enumValueWire(ev) == s || string(ev.Name()) == s {
				return protoreflect.ValueOfEnum(ev.Number()), nil
			}
		}
		// A value the schema did not list (a newer upstream): the zero value,
		// as the JSON decoder treats one.
		return protoreflect.ValueOfEnum(0), nil
	case protoreflect.MessageKind:
		if fd.Message().FullName() == timestampName {
			t, err := parseXSDDateTime(s)
			if err != nil {
				return bad(err)
			}
			return protoreflect.ValueOfMessage(timestamppb.New(t).ProtoReflect()), nil
		}
	}
	return bad(fmt.Errorf("no XML form for %s", fd.Kind()))
}

// parseXSDDateTime reads an xsd:dateTime; one without a zone is UTC.
func parseXSDDateTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("not an xsd:dateTime")
}

// Fault is a SOAP Fault: Client (SOAP 1.1) / Sender (1.2) blames the
// request, anything else the service.
type Fault struct {
	Code   string // the code's local part: Client, Server, Sender, Receiver, …
	Reason string // faultstring / Reason/Text
	Detail string // the detail element's text, trimmed
}

// ParseFault reads a Fault out of an envelope; ok is false when the body
// carries none.
func ParseFault(raw []byte) (f Fault, ok bool) {
	d := xml.NewDecoder(bytes.NewReader(raw))
	if toBody(d) != nil {
		return Fault{}, false
	}
	var inFault bool
	var path []string
	var detail strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return f, inFault
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !inFault {
				if t.Name.Local != "Fault" {
					return Fault{}, false
				}
				inFault = true
				continue
			}
			path = append(path, t.Name.Local)
		case xml.EndElement:
			if len(path) == 0 {
				return f, inFault
			}
			path = path[:len(path)-1]
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text == "" || len(path) == 0 {
				continue
			}
			switch {
			case path[0] == "faultcode", path[0] == "Code" && path[len(path)-1] == "Value" && f.Code == "":
				if i := strings.LastIndex(text, ":"); i >= 0 {
					text = text[i+1:]
				}
				f.Code = text
			case path[0] == "faultstring", path[0] == "Reason" && path[len(path)-1] == "Text":
				f.Reason = text
			case path[0] == "detail", path[0] == "Detail":
				if detail.Len() > 0 {
					detail.WriteByte(' ')
				}
				detail.WriteString(text)
				f.Detail = detail.String()
			}
		}
	}
}

// ClientFault reports whether the fault blames the request.
func (f Fault) ClientFault() bool { return f.Code == "Client" || f.Code == "Sender" }
