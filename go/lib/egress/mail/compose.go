package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"time"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// now is the Date header's clock; a variable for tests.
var now = time.Now

// message is a MailMessage checked and resolved: every address parsed, the
// sender chosen, the Message-ID made.
type message struct {
	in        *w17pb.MailMessage
	from      *mail.Address
	to, cc    []*mail.Address
	bcc       []*mail.Address
	replyTo   []*mail.Address
	messageID string
}

// recipients is every envelope recipient: to, cc and bcc.
func (m *message) recipients() []string {
	var out []string
	for _, l := range [][]*mail.Address{m.to, m.cc, m.bcc} {
		for _, a := range l {
			out = append(out, a.Address)
		}
	}
	return out
}

// reserved are the headers a message sets from its own fields.
var reserved = map[string]bool{"From": true, "To": true, "Cc": true, "Bcc": true, "Reply-To": true,
	"Subject": true, "Date": true, "Message-Id": true, "Mime-Version": true, "Sender": true,
	"Content-Type": true, "Content-Transfer-Encoding": true, "Content-Disposition": true, "Content-Id": true}

// prepare checks in and resolves it against the DSN's default sender. Its
// error is the caller's: INVALID_ARGUMENT.
func prepare(in *w17pb.MailMessage, defaultFrom string) (*message, error) {
	m := &message{in: in}
	from := in.GetFrom()
	if from == "" {
		from = defaultFrom
	}
	if from == "" {
		return nil, errors.New("the message has no sender: set from, or from= in the DSN")
	}
	var err error
	if m.from, err = parseAddress(from); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	for _, f := range []struct {
		name string
		in   []string
		out  *[]*mail.Address
	}{{"to", in.GetTo(), &m.to}, {"cc", in.GetCc(), &m.cc}, {"bcc", in.GetBcc(), &m.bcc}, {"reply_to", in.GetReplyTo(), &m.replyTo}} {
		for i, s := range f.in {
			a, err := parseAddress(s)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", f.name, i, err)
			}
			*f.out = append(*f.out, a)
		}
	}
	if len(m.to)+len(m.cc)+len(m.bcc) == 0 {
		return nil, errors.New("the message has no recipient: set to, cc or bcc")
	}
	if in.GetText() == "" && in.GetHtml() == "" {
		return nil, errors.New("the message has no body: set text, html or both")
	}
	if err := headerSafe("subject", in.GetSubject()); err != nil {
		return nil, err
	}
	size := len(in.GetText()) + len(in.GetHtml())
	cids := map[string]bool{}
	for i, a := range in.GetAttachments() {
		if a.GetFilename() == "" {
			return nil, fmt.Errorf("attachments[%d]: no filename", i)
		}
		if err := headerSafe(fmt.Sprintf("attachments[%d].filename", i), a.GetFilename()); err != nil {
			return nil, err
		}
		if ct := a.GetContentType(); ct != "" {
			if _, _, err := mime.ParseMediaType(ct); err != nil {
				return nil, fmt.Errorf("attachments[%d].content_type %q: %w", i, ct, err)
			}
		}
		if id := a.GetContentId(); id != "" {
			if strings.ContainsAny(id, "<> \t\r\n") {
				return nil, fmt.Errorf("attachments[%d].content_id: write it bare, as the HTML's cid: refers to it", i)
			}
			if cids[id] {
				return nil, fmt.Errorf("attachments[%d].content_id %q repeats", i, id)
			}
			cids[id] = true
		}
		size += len(a.GetContent())
	}
	if size > MaxMessageBytes {
		return nil, fmt.Errorf("the message is %d bytes, over the %d-byte cap", size, MaxMessageBytes)
	}
	for k, v := range in.GetHeaders() {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if reserved[ck] {
			return nil, fmt.Errorf("headers[%q]: the message sets it from its own fields", k)
		}
		if k == "" || strings.ContainsAny(k, ": \t\r\n") {
			return nil, fmt.Errorf("headers: %q is not a header name", k)
		}
		if err := headerSafe("headers["+k+"]", v); err != nil {
			return nil, err
		}
	}
	m.messageID = newMessageID(m.from.Address)
	return m, nil
}

func parseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("an address does not span lines")
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not an e-mail address", s)
	}
	return a, nil
}

// headerSafe refuses a value that would end its header line: the way a
// header is injected.
func headerSafe(field, v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("%s spans lines; a header is one", field)
	}
	return nil
}

func newMessageID(from string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	domain := "localhost"
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i < len(from)-1 {
		domain = from[i+1:]
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

func contentType(a *w17pb.MailAttachment) string {
	if ct := a.GetContentType(); ct != "" {
		return ct
	}
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(a.GetFilename()))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func addressList(l []*mail.Address) string {
	s := make([]string, len(l))
	for i, a := range l {
		s[i] = formatAddress(a)
	}
	return strings.Join(s, ", ")
}

// formatAddress is a, bare when it has no display name.
func formatAddress(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.String()
}

// compose renders m as an RFC 5322 message, MIME encoded. Bcc is never
// written: it travels on the envelope only.
func compose(m *message) []byte {
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", formatAddress(m.from))
	if len(m.to) > 0 {
		h("To", addressList(m.to))
	}
	if len(m.cc) > 0 {
		h("Cc", addressList(m.cc))
	}
	if len(m.replyTo) > 0 {
		h("Reply-To", addressList(m.replyTo))
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.in.GetSubject()))
	h("Date", now().Format(time.RFC1123Z))
	h("Message-ID", m.messageID)
	h("MIME-Version", "1.0")
	for _, k := range sortedKeys(m.in.GetHeaders()) {
		h(textproto.CanonicalMIMEHeaderKey(k), mime.QEncoding.Encode("utf-8", m.in.GetHeaders()[k]))
	}

	var inline, files []*w17pb.MailAttachment
	for _, a := range m.in.GetAttachments() {
		if a.GetContentId() != "" {
			inline = append(inline, a)
		} else {
			files = append(files, a)
		}
	}
	body := part(func(w *bytes.Buffer) textproto.MIMEHeader { return writeBody(w, m.in.GetText(), m.in.GetHtml()) })
	if len(inline) > 0 {
		body = multi("related", append([]part{body}, attachmentParts(inline, "inline")...))
	}
	if len(files) > 0 {
		body = multi("mixed", append([]part{body}, attachmentParts(files, "attachment")...))
	}
	var content bytes.Buffer
	hdr := body(&content)
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if v := hdr.Get(k); v != "" {
			h(k, v)
		}
	}
	b.WriteString("\r\n")
	b.Write(content.Bytes())
	return b.Bytes()
}

// part writes one MIME part's content to w and returns its headers.
type part func(w *bytes.Buffer) textproto.MIMEHeader

func multi(kind string, parts []part) part {
	return func(w *bytes.Buffer) textproto.MIMEHeader {
		mw := multipart.NewWriter(w)
		for _, p := range parts {
			var content bytes.Buffer
			hdr := p(&content)
			pw, _ := mw.CreatePart(hdr)
			_, _ = pw.Write(content.Bytes())
		}
		_ = mw.Close()
		return textproto.MIMEHeader{"Content-Type": {"multipart/" + kind + "; boundary=" + mw.Boundary()}}
	}
}

func writeBody(w *bytes.Buffer, text, html string) textproto.MIMEHeader {
	switch {
	case text != "" && html != "":
		return multi("alternative", []part{
			func(w *bytes.Buffer) textproto.MIMEHeader { return textPart(w, "text/plain", text) },
			func(w *bytes.Buffer) textproto.MIMEHeader { return textPart(w, "text/html", html) },
		})(w)
	case html != "":
		return textPart(w, "text/html", html)
	}
	return textPart(w, "text/plain", text)
}

func textPart(w *bytes.Buffer, mediaType, s string) textproto.MIMEHeader {
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(s))
	_ = qp.Close()
	return textproto.MIMEHeader{
		"Content-Type":              {mediaType + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	}
}

func attachmentParts(l []*w17pb.MailAttachment, disposition string) []part {
	out := make([]part, len(l))
	for i, a := range l {
		out[i] = func(w *bytes.Buffer) textproto.MIMEHeader {
			writeBase64(w, a.GetContent())
			hdr := textproto.MIMEHeader{
				"Content-Type":              {mime.FormatMediaType(contentType(a), map[string]string{"name": a.GetFilename()})},
				"Content-Transfer-Encoding": {"base64"},
				"Content-Disposition":       {mime.FormatMediaType(disposition, map[string]string{"filename": a.GetFilename()})},
			}
			if id := a.GetContentId(); id != "" {
				hdr["Content-Id"] = []string{"<" + id + ">"}
			}
			return hdr
		}
	}
	return out
}

// writeBase64 writes b in lines of 76 characters, as RFC 2045 asks.
func writeBase64(w *bytes.Buffer, b []byte) {
	enc := base64.StdEncoding.EncodeToString(b)
	for len(enc) > 76 {
		w.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	if enc != "" {
		w.WriteString(enc + "\r\n")
	}
}

func sortedKeys(h map[string]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
