package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
)

// apiSender sends through a transactional-mail API over HTTPS.
type apiSender struct {
	client string
	dsn    *DSN
	http   *http.Client
}

func (a *apiSender) wantsMIME() bool { return a.dsn.Provider == Mailgun }

func (d *DSN) baseURL() string {
	if d.Endpoint != "" {
		return d.Endpoint
	}
	switch d.Provider {
	case Postmark:
		return "https://api.postmarkapp.com"
	case SendGrid:
		return "https://api.sendgrid.com"
	case Mailgun:
		if d.Region == "eu" {
			return "https://api.eu.mailgun.net"
		}
		return "https://api.mailgun.net"
	case Resend:
		return "https://api.resend.com"
	}
	return ""
}

func (a *apiSender) send(ctx context.Context, m *message, raw []byte) (string, outcome) {
	req, err := a.request(ctx, m, raw)
	if err != nil {
		return "", final(callError(codes.InvalidArgument, a.client, ReasonInvalid, err.Error(), nil))
	}
	resp, err := a.http.Do(req)
	if err != nil {
		e := transportFailure(a.client, transportText(err))
		if neverSent(err) || a.idempotent() {
			return "", notSent(e)
		}
		return "", final(callError(codes.Unavailable, a.client, ReasonOutcomeUnknown,
			"the request failed before the provider answered: the message may have been accepted — not retried, to send no second copy",
			map[string]string{"cause": transportText(err)}))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return a.providerID(resp, body), sent()
	}
	meta := map[string]string{"http_status": strconv.Itoa(resp.StatusCode)}
	if len(body) > 0 {
		meta["body"] = truncate(body, maxErrorBody)
	}
	e := callError(codeFor(resp.StatusCode), a.client, "UPSTREAM_HTTP_"+strconv.Itoa(resp.StatusCode),
		"the provider answered HTTP "+strconv.Itoa(resp.StatusCode), meta)
	// 429 and 503 say the request was not taken; with an idempotency key
	// any 5xx may be repeated.
	if resp.StatusCode == 429 || resp.StatusCode == 503 || (resp.StatusCode >= 500 && a.idempotent()) {
		return "", notSent(e).after(retryAfter(resp.Header.Get("Retry-After")))
	}
	return "", final(e)
}

// idempotent: the provider takes an idempotency key, so a repeated request
// never sends twice.
func (a *apiSender) idempotent() bool { return a.dsn.Provider == Resend }

// neverSent: the connection was never made, so the request never left.
func neverSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func codeFor(s int) codes.Code {
	switch {
	case s == 400, s == 422:
		return codes.InvalidArgument
	case s == 401, s == 403:
		// Our key, never the caller's fault.
		return codes.Internal
	case s == 429, s >= 500:
		return codes.Unavailable
	}
	return codes.Internal
}

func retryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func (a *apiSender) request(ctx context.Context, m *message, raw []byte) (*http.Request, error) {
	base := a.dsn.baseURL()
	var (
		target, ctype string
		body          []byte
		err           error
	)
	switch a.dsn.Provider {
	case Postmark:
		target, ctype = base+"/email", "application/json"
		body, err = json.Marshal(postmarkBody(m, a.dsn.Stream))
	case SendGrid:
		target, ctype = base+"/v3/mail/send", "application/json"
		body, err = json.Marshal(sendgridBody(m))
	case Resend:
		target, ctype = base+"/emails", "application/json"
		body, err = json.Marshal(resendBody(m))
	case Mailgun:
		target = base + "/v3/" + a.dsn.Host + "/messages.mime"
		body, ctype, err = mailgunBody(m, raw)
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Accept", "application/json")
	switch a.dsn.Provider {
	case Postmark:
		req.Header.Set("X-Postmark-Server-Token", a.dsn.Password)
	case SendGrid, Resend:
		req.Header.Set("Authorization", "Bearer "+a.dsn.Password)
	case Mailgun:
		req.SetBasicAuth("api", a.dsn.Password)
	}
	if a.idempotent() {
		// One key per message: every attempt of the call repeats it.
		req.Header.Set("Idempotency-Key", strings.Trim(m.messageID, "<>"))
	}
	return req, nil
}

func (a *apiSender) providerID(resp *http.Response, body []byte) string {
	if a.dsn.Provider == SendGrid {
		return resp.Header.Get("X-Message-Id")
	}
	var r struct {
		MessageID string `json:"MessageID"`
		ID        string `json:"id"`
	}
	_ = json.Unmarshal(body, &r)
	if a.dsn.Provider == Postmark {
		return r.MessageID
	}
	return r.ID
}

func joinAddrs(l []*mail.Address) string { return addressList(l) }

func postmarkBody(m *message, stream string) map[string]any {
	b := map[string]any{"From": formatAddress(m.from), "Subject": m.in.GetSubject()}
	for k, l := range map[string][]*mail.Address{"To": m.to, "Cc": m.cc, "Bcc": m.bcc, "ReplyTo": m.replyTo} {
		if len(l) > 0 {
			b[k] = joinAddrs(l)
		}
	}
	if t := m.in.GetText(); t != "" {
		b["TextBody"] = t
	}
	if h := m.in.GetHtml(); h != "" {
		b["HtmlBody"] = h
	}
	if stream != "" {
		b["MessageStream"] = stream
	}
	var hs []map[string]string
	for _, k := range sortedKeys(m.in.GetHeaders()) {
		hs = append(hs, map[string]string{"Name": k, "Value": m.in.GetHeaders()[k]})
	}
	if hs != nil {
		b["Headers"] = hs
	}
	var at []map[string]string
	for _, x := range m.in.GetAttachments() {
		e := map[string]string{"Name": x.GetFilename(), "Content": base64.StdEncoding.EncodeToString(x.GetContent()), "ContentType": contentType(x)}
		if id := x.GetContentId(); id != "" {
			e["ContentID"] = "cid:" + id
		}
		at = append(at, e)
	}
	if at != nil {
		b["Attachments"] = at
	}
	return b
}

type sgAddr struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

func sgAddrs(l []*mail.Address) []sgAddr {
	out := make([]sgAddr, len(l))
	for i, a := range l {
		out[i] = sgAddr{a.Address, a.Name}
	}
	return out
}

func sendgridBody(m *message) map[string]any {
	p := map[string]any{}
	for k, l := range map[string][]*mail.Address{"to": m.to, "cc": m.cc, "bcc": m.bcc} {
		if len(l) > 0 {
			p[k] = sgAddrs(l)
		}
	}
	b := map[string]any{
		"personalizations": []any{p},
		"from":             sgAddr{m.from.Address, m.from.Name},
		"subject":          m.in.GetSubject(),
	}
	if len(m.replyTo) > 0 {
		b["reply_to_list"] = sgAddrs(m.replyTo)
	}
	// text/plain must come first.
	var content []map[string]string
	if t := m.in.GetText(); t != "" {
		content = append(content, map[string]string{"type": "text/plain", "value": t})
	}
	if h := m.in.GetHtml(); h != "" {
		content = append(content, map[string]string{"type": "text/html", "value": h})
	}
	b["content"] = content
	if len(m.in.GetHeaders()) > 0 {
		b["headers"] = m.in.GetHeaders()
	}
	var at []map[string]string
	for _, x := range m.in.GetAttachments() {
		e := map[string]string{"content": base64.StdEncoding.EncodeToString(x.GetContent()), "filename": x.GetFilename(),
			"type": contentType(x), "disposition": "attachment"}
		if id := x.GetContentId(); id != "" {
			e["disposition"], e["content_id"] = "inline", id
		}
		at = append(at, e)
	}
	if at != nil {
		b["attachments"] = at
	}
	return b
}

func addrStrings(l []*mail.Address) []string {
	out := make([]string, len(l))
	for i, a := range l {
		out[i] = formatAddress(a)
	}
	return out
}

func resendBody(m *message) map[string]any {
	b := map[string]any{"from": formatAddress(m.from), "subject": m.in.GetSubject()}
	for k, l := range map[string][]*mail.Address{"to": m.to, "cc": m.cc, "bcc": m.bcc, "reply_to": m.replyTo} {
		if len(l) > 0 {
			b[k] = addrStrings(l)
		}
	}
	if t := m.in.GetText(); t != "" {
		b["text"] = t
	}
	if h := m.in.GetHtml(); h != "" {
		b["html"] = h
	}
	if len(m.in.GetHeaders()) > 0 {
		b["headers"] = m.in.GetHeaders()
	}
	var at []map[string]string
	for _, x := range m.in.GetAttachments() {
		e := map[string]string{"filename": x.GetFilename(), "content": base64.StdEncoding.EncodeToString(x.GetContent()), "content_type": contentType(x)}
		if id := x.GetContentId(); id != "" {
			e["content_id"] = id
		}
		at = append(at, e)
	}
	if at != nil {
		b["attachments"] = at
	}
	return b
}

// mailgunBody is the MIME endpoint's form: every envelope recipient as a
// `to` (Bcc included — the message itself never names them) and the
// composed message as the `message` file.
func mailgunBody(m *message, raw []byte) ([]byte, string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, r := range m.recipients() {
		if err := w.WriteField("to", r); err != nil {
			return nil, "", err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="message"; filename="message.mime"`)
	h.Set("Content-Type", "message/rfc822")
	fw, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(raw); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), w.FormDataContentType(), nil
}
