package wire

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// Request is a call's request message taken apart the way the upstream
// takes it: each field goes where its (w17.egress_field).in says — into the
// path template, the query, a header or the body.
type Request struct {
	// Path holds each path parameter's text, by the name the template
	// writes it under (`{order_id}`), not yet escaped.
	Path map[string]string
	// Query holds the query parameters. A repeated field repeats its
	// parameter; a message is a deep object (`created[gte]=1`).
	Query url.Values
	// Header holds the header parameters, by their upstream names.
	Header http.Header

	msg  protoreflect.Message
	body *fieldPlan
}

// Split takes m apart. A field with no place on the request (no `in`), or
// a path or header parameter that is not a scalar, is an error: the
// converter never emits one, and guessing would send a different call.
func Split(m proto.Message) (*Request, error) {
	msg := m.ProtoReflect()
	r := &Request{Path: map[string]string{}, Query: url.Values{}, Header: http.Header{}, msg: msg}
	for _, fp := range planFor(msg.Descriptor()).fields {
		fd := fp.fd
		switch fp.in {
		case w17pb.EgressIn_EGRESS_IN_PATH:
			s, err := scalarText(msg, fp)
			if err != nil {
				return nil, err
			}
			r.Path[fp.name] = s
		case w17pb.EgressIn_EGRESS_IN_HEADER:
			if !emits(msg, fp) {
				continue
			}
			if fd.IsMap() || (fd.Kind() == protoreflect.MessageKind && !isWrapper(fd.Message())) {
				return nil, fmt.Errorf("header parameter %q: a %s has no header form", fp.name, kindName(fd))
			}
			vals := url.Values{}
			if err := appendQueryField(vals, fp.name, msg, fp); err != nil {
				return nil, err
			}
			for _, v := range vals[fp.name] {
				r.Header.Add(fp.name, v)
			}
		case w17pb.EgressIn_EGRESS_IN_QUERY:
			if !emits(msg, fp) {
				continue
			}
			if err := appendQueryField(r.Query, fp.name, msg, fp); err != nil {
				return nil, err
			}
		case w17pb.EgressIn_EGRESS_IN_BODY:
			r.body = fp
		default:
			return nil, fmt.Errorf("field %s has no place on the request: (w17.egress_field).in is unset", fd.FullName())
		}
	}
	return r, nil
}

// HasBody reports whether the call sends a body: the message has a body
// field and it is set (or required).
func (r *Request) HasBody() bool {
	return r.body != nil && emits(r.msg, r.body)
}

// JSON renders the body as the upstream's JSON.
func (r *Request) JSON() ([]byte, error) {
	if !r.HasBody() {
		return nil, nil
	}
	var b bytes.Buffer
	if err := writeFieldValue(&b, r.msg, r.body, ""); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Form renders the body as an application/x-www-form-urlencoded body.
func (r *Request) Form() (url.Values, error) {
	vals := url.Values{}
	if !r.HasBody() {
		return vals, nil
	}
	if err := appendFormField(vals, "", r.msg, r.body); err != nil {
		return nil, err
	}
	return vals, nil
}

// appendQueryField writes one query (or header) parameter. A repeated field
// repeats the parameter rather than indexing it — `expand=a&expand=b` is
// how a query array is written; a message is a deep object under its name.
func appendQueryField(vals url.Values, name string, msg protoreflect.Message, fp *fieldPlan) error {
	fd := fp.fd
	if fd.IsList() {
		l := msg.Get(fd).List()
		for i := 0; i < l.Len(); i++ {
			if err := appendFormValue(vals, name, fd, l.Get(i)); err != nil {
				return err
			}
		}
		return nil
	}
	return appendFormField(vals, name, msg, fp)
}

// scalarText is a path parameter's text.
func scalarText(msg protoreflect.Message, fp *fieldPlan) (string, error) {
	fd := fp.fd
	if fd.IsList() || fd.IsMap() || fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		return "", fmt.Errorf("path parameter %q: a %s has no path form", fp.name, kindName(fd))
	}
	vals := url.Values{}
	if err := appendFormValue(vals, fp.name, fd, msg.Get(fd)); err != nil {
		return "", err
	}
	return vals.Get(fp.name), nil
}

func kindName(fd protoreflect.FieldDescriptor) string {
	switch {
	case fd.IsMap():
		return "map"
	case fd.IsList():
		return "list"
	}
	return fd.Kind().String()
}
