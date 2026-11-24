package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

func init() {
	sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
}

const clientSrc = `syntax = "proto3";
package clients.mail;
import "w17/egress.proto";
import "w17/mail.proto";
option (w17.egress_client) = { name: "mail", mail: true };
service MailApi {
  rpc Send(w17.MailMessage) returns (w17.MailReceipt);
}
`

func mailService(t *testing.T) protoreflect.ServiceDescriptor {
	t.Helper()
	const path = "clients/mail/mail.proto"
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(protocompile.ResolverFunc(func(p string) (protocompile.SearchResult, error) {
		if p == path {
			return protocompile.SearchResult{Source: strings.NewReader(clientSrc)}, nil
		}
		fd, err := protoregistry.GlobalFiles.FindFileByPath(p)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		return protocompile.SearchResult{Desc: fd}, nil
	}))}
	files, err := c.Compile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return files[0].Services().ByName("MailApi")
}

func send(t *testing.T, dsn string, msg *w17pb.MailMessage, opts ...Option) (*w17pb.MailReceipt, error) {
	t.Helper()
	conn := inprocgrpc.New()
	lookup := func(string) (string, bool) { return dsn, dsn != "" }
	if err := RegisterFromEnv(conn, mailService(t), "mail", "W17_CLIENT_MAIL_DSN", lookup, opts...); err != nil {
		t.Fatal(err)
	}
	out := &w17pb.MailReceipt{}
	err := conn.Invoke(context.Background(), "/clients.mail.MailApi/Send", msg, out)
	return out, err
}

func reason(err error) (codes.Code, string, map[string]string) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), i.GetReason(), i.GetMetadata()
		}
	}
	return st.Code(), "", nil
}

func invoice() *w17pb.MailMessage {
	return &w17pb.MailMessage{
		To:      []string{"Jana Nováková <jana@example.com>"},
		Bcc:     []string{"archive@example.com"},
		Subject: "Faktura 2026-0042 — děkujeme",
		Text:    "Dobrý den,\nv příloze je faktura.\n.\nS pozdravem",
		Html:    "<p>Dobrý den,</p><p>v příloze je faktura.</p><img src=\"cid:logo\">",
		Attachments: []*w17pb.MailAttachment{
			{Filename: "faktura-2026-0042.pdf", Content: []byte("%PDF-1.7 fake\x00\x01")},
			{Filename: "faktura.isdoc", ContentType: "application/xml", Content: []byte("<Invoice/>")},
			{Filename: "logo.png", ContentId: "logo", Content: []byte("\x89PNG")},
		},
		Headers: map[string]string{"X-Entity-Ref-ID": "inv-42"},
	}
}

func TestParseDSN(t *testing.T) {
	ok := map[string]func(*DSN) bool{
		"smtps://u:p%40ss@smtp.example.com":                            func(d *DSN) bool { return d.Port == "465" && d.Password == "p@ss" },
		"smtp://u:pw0rd@smtp.example.com?from=Faktury+<f@example.com>": func(d *DSN) bool { return d.Port == "587" && d.From == "Faktury <f@example.com>" },
		"smtp://mailpit:1025?tls=none":                                 func(d *DSN) bool { return d.PlainText && d.User == "" },
		"smtp://u:pw0rd@127.0.0.1:1025?tls=none":                       func(d *DSN) bool { return d.PlainText },
		"postmark://tok@?stream=broadcast":                             func(d *DSN) bool { return d.Password == "tok" && d.Stream == "broadcast" },
		"mailgun://key@mg.example.com?region=eu":                       func(d *DSN) bool { return d.baseURL() == "https://api.eu.mailgun.net" },
		"resend://re_123@?timeout=5s&max_attempts=5":                   func(d *DSN) bool { return d.timeout() == 5*time.Second && d.maxAttempts() == 5 },
		"sendgrid://SG.x@":                                             func(d *DSN) bool { return d.Server() == "api.sendgrid.com" },
	}
	for s, check := range ok {
		d, err := ParseDSN(s)
		if err != nil || !check(d) {
			t.Errorf("%s: %+v, %v", s, d, err)
		}
		if d != nil && strings.Contains(d.String(), d.Password) && d.Password != "" {
			t.Errorf("%s: String() prints the credential: %s", s, d)
		}
	}
	bad := []string{
		"", "http://x", "smtp://", "smtp://u@h", "smtp://u:p@smtp.example.com?tls=none",
		"smtps://h?tls=none", "smtp://h?tls=maybe", "postmark://", "postmark://t@host",
		"mailgun://k@", "sendgrid://k@?region=eu", "resend://k@?endpoint=http://api.example.com",
		"smtp://h?from=nope", "smtp://h?timeout=2m", "smtp://h?max_attempts=0", "smtp://h?colour=red",
	}
	for _, s := range bad {
		if _, err := ParseDSN(s); err == nil {
			t.Errorf("%q parsed", s)
		} else if strings.Contains(err.Error(), "p@") {
			t.Errorf("%q: error quotes the DSN: %v", s, err)
		}
	}
}

func TestPrepareRefuses(t *testing.T) {
	cases := map[string]func(m *w17pb.MailMessage){
		"no sender":       func(m *w17pb.MailMessage) {},
		"no recipient":    func(m *w17pb.MailMessage) { m.From, m.To, m.Bcc = "a@example.com", nil, nil },
		"no body":         func(m *w17pb.MailMessage) { m.From, m.Text, m.Html = "a@example.com", "", "" },
		"bad address":     func(m *w17pb.MailMessage) { m.From, m.To = "a@example.com", []string{"nope"} },
		"subject newline": func(m *w17pb.MailMessage) { m.From, m.Subject = "a@example.com", "x\r\nBcc: evil@example.com" },
		"reserved header": func(m *w17pb.MailMessage) { m.From, m.Headers = "a@example.com", map[string]string{"bcc": "x"} },
		"header newline":  func(m *w17pb.MailMessage) { m.From, m.Headers = "a@example.com", map[string]string{"X-A": "1\nB: 2"} },
		"no filename":     func(m *w17pb.MailMessage) { m.From = "a@example.com"; m.Attachments[0].Filename = "" },
		"too large": func(m *w17pb.MailMessage) {
			m.From = "a@example.com"
			m.Attachments[0].Content = make([]byte, MaxMessageBytes)
		},
	}
	for name, mutate := range cases {
		m := invoice()
		mutate(m)
		if _, err := prepare(m, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Through the service, a refusal is INVALID_ARGUMENT and nothing is sent.
	_, err := send(t, "smtp://127.0.0.1:1?tls=none", invoice())
	if c, r, _ := reason(err); c != codes.InvalidArgument || r != ReasonInvalid {
		t.Fatalf("%v", err)
	}
}

// readParts walks a composed message: every leaf part's media type and
// decoded content, and the headers of the top.
func readParts(t *testing.T, raw []byte) (netmail.Header, map[string][]byte, map[string]string) {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	leaves, disp := map[string][]byte{}, map[string]string{}
	var walk func(ct string, h map[string][]string, body io.Reader)
	walk = func(ct string, h map[string][]string, body io.Reader) {
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(mt, "multipart/") {
			r := multipart.NewReader(body, params["boundary"])
			for {
				p, err := r.NextRawPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				walk(p.Header.Get("Content-Type"), p.Header, p)
			}
		}
		b, _ := io.ReadAll(body)
		switch strings.ToLower(first(h["Content-Transfer-Encoding"])) {
		case "base64":
			b, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(string(b), "\r\n", ""))
			if err != nil {
				t.Fatal(err)
			}
		case "quoted-printable":
			b, _ = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		}
		key := mt
		if fn := params["name"]; fn != "" {
			key = fn
		}
		leaves[key] = b
		disp[key] = first(h["Content-Disposition"]) + " " + first(h["Content-Id"])
	}
	walk(msg.Header.Get("Content-Type"), msg.Header, msg.Body)
	return msg.Header, leaves, disp
}

func first(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func TestCompose(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	m, err := prepare(invoice(), "Faktury <faktury@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	raw := compose(m)
	h, leaves, disp := readParts(t, raw)
	if h.Get("Bcc") != "" || bytes.Contains(raw, []byte("archive@")) {
		t.Fatal("Bcc written into the message")
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(h.Get("Subject")); subj != "Faktura 2026-0042 — děkujeme" {
		t.Fatalf("subject %q", subj)
	}
	if !strings.HasSuffix(h.Get("Message-Id"), "@example.com>") || h.Get("X-Entity-Ref-Id") != "inv-42" {
		t.Fatalf("headers %v", h)
	}
	if to, _ := h.AddressList("To"); len(to) != 1 || to[0].Name != "Jana Nováková" {
		t.Fatalf("To %v", to)
	}
	if string(leaves["text/plain"]) != "Dobrý den,\r\nv příloze je faktura.\r\n.\r\nS pozdravem" {
		t.Fatalf("text %q", leaves["text/plain"])
	}
	if !bytes.Contains(leaves["text/html"], []byte("cid:logo")) {
		t.Fatalf("html %q", leaves["text/html"])
	}
	if string(leaves["faktura-2026-0042.pdf"]) != "%PDF-1.7 fake\x00\x01" || string(leaves["faktura.isdoc"]) != "<Invoice/>" {
		t.Fatalf("attachments %q", leaves)
	}
	if !strings.HasPrefix(disp["logo.png"], "inline") || !strings.Contains(disp["logo.png"], "<logo>") ||
		!strings.HasPrefix(disp["faktura-2026-0042.pdf"], "attachment") {
		t.Fatalf("dispositions %v", disp)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a line of %d bytes", len(line))
		}
	}
}

// smtpServer is a scripted SMTP server: replies overrides the reply to a
// command (by its verb) on the given connection number.
type smtpServer struct {
	addr string
	mu   sync.Mutex
	// per connection: the verb → reply overrides; "DROP" closes after the
	// message's final dot without answering.
	script   []map[string]string
	conns    int
	from     string
	rcpt     []string
	data     []byte
	tls      *tls.Config
	starttls bool
}

func newSMTP(t *testing.T, implicitTLS bool, script ...map[string]string) *smtpServer {
	t.Helper()
	s := &smtpServer{script: script}
	var ln net.Listener
	var err error
	if implicitTLS {
		s.tls = serverTLS(t)
		ln, err = tls.Listen("tcp", "127.0.0.1:0", s.tls)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	s.mu.Lock()
	n := s.conns
	s.conns++
	over := map[string]string{}
	if n < len(s.script) {
		over = s.script[n]
	}
	s.mu.Unlock()
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(l string) { _, _ = w.WriteString(l + "\r\n"); _ = w.Flush() }
	reply := func(verb, def string) bool {
		if o, ok := over[verb]; ok {
			say(o)
			return strings.HasPrefix(o, "2") || strings.HasPrefix(o, "3")
		}
		say(def)
		return true
	}
	say("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO":
			ext := "250-test\r\n250 AUTH PLAIN"
			if s.starttls {
				ext = "250-test\r\n250-STARTTLS\r\n250 AUTH PLAIN"
			}
			say(ext)
		case "AUTH":
			reply("AUTH", "235 ok")
		case "MAIL":
			if reply("MAIL", "250 ok") {
				s.mu.Lock()
				s.from = line
				s.mu.Unlock()
			}
		case "RCPT":
			if reply("RCPT", "250 ok") {
				s.mu.Lock()
				s.rcpt = append(s.rcpt, strings.TrimPrefix(line, "RCPT TO:"))
				s.mu.Unlock()
			}
		case "DATA":
			if !reply("DATA", "354 go") {
				continue
			}
			var b bytes.Buffer
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			if over["END"] == "DROP" {
				return
			}
			if reply("END", "250 queued") {
				s.mu.Lock()
				s.data = b.Bytes()
				s.mu.Unlock()
			}
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 no")
		}
	}
}

func serverTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func clientTLS(s *smtpServer) *tls.Config {
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(s.tls.Certificates[0].Certificate[0])
	pool.AddCert(leaf)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func TestSMTPSends(t *testing.T) {
	s := newSMTP(t, false)
	r, err := send(t, "smtp://u:p@"+s.addr+"?tls=none&from=Faktury+<faktury@example.com>", invoice())
	if err != nil {
		t.Fatal(err)
	}
	if r.GetAttempts() != 1 || !strings.HasSuffix(r.GetMessageId(), "@example.com>") {
		t.Fatalf("receipt %v", r)
	}
	if !strings.Contains(s.from, "<faktury@example.com>") || len(s.rcpt) != 2 || !strings.Contains(s.rcpt[1], "archive@example.com") {
		t.Fatalf("envelope %q %q", s.from, s.rcpt)
	}
	h, leaves, _ := readParts(t, s.data)
	if h.Get("Message-Id") != r.GetMessageId() || string(leaves["faktura.isdoc"]) != "<Invoice/>" {
		t.Fatalf("delivered %v", h)
	}
	// A dot-led line of the body came through unstuffed.
	if !bytes.Contains(leaves["text/plain"], []byte("\r\n.\r\n")) {
		t.Fatalf("dot line lost: %q", leaves["text/plain"])
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	s := newSMTP(t, true)
	if _, err := send(t, "smtps://u:p@"+s.addr+"?from=a@example.com", invoice(), WithTLSConfig(clientTLS(s))); err != nil {
		t.Fatal(err)
	}
	// An unknown certificate is refused, and as a transport failure.
	s2 := newSMTP(t, true)
	_, err := send(t, "smtps://u:p@"+s2.addr+"?from=a@example.com&max_attempts=1", invoice())
	if c, r, _ := reason(err); c != codes.Unavailable || r != ReasonTransport {
		t.Fatalf("%v", err)
	}
}

func TestSMTPRequiresSTARTTLS(t *testing.T) {
	s := newSMTP(t, false)
	_, err := send(t, "smtp://u:p@"+s.addr+"?from=a@example.com", invoice())
	if c, r, _ := reason(err); c != codes.Internal || r != ReasonTLSUnavailable {
		t.Fatalf("%v", err)
	}
	if s.data != nil {
		t.Fatal("sent in clear text")
	}
}

func TestSMTPTemporaryIsRetried(t *testing.T) {
	s := newSMTP(t, false, map[string]string{"RCPT": "451 try later"})
	r, err := send(t, "smtp://"+s.addr+"?tls=none&from=a@example.com", invoice())
	if err != nil || r.GetAttempts() != 2 {
		t.Fatalf("%v %v", r, err)
	}
}

func TestSMTPPermanentIsNot(t *testing.T) {
	s := newSMTP(t, false, map[string]string{"RCPT": "550 no such user"})
	_, err := send(t, "smtp://"+s.addr+"?tls=none&from=a@example.com", invoice())
	c, r, meta := reason(err)
	if c != codes.InvalidArgument || r != "UPSTREAM_SMTP_550" || meta["attempts"] != "1" || s.conns != 1 {
		t.Fatalf("%v %v conns=%d", err, meta, s.conns)
	}
	s = newSMTP(t, false, map[string]string{"AUTH": "535 bad credentials"})
	_, err = send(t, "smtp://u:p@"+s.addr+"?tls=none&from=a@example.com", invoice())
	if c, r, _ := reason(err); c != codes.Internal || r != "UPSTREAM_SMTP_535" || strings.Contains(err.Error(), "u:p") {
		t.Fatalf("%v", err)
	}
}

func TestSMTPUnknownOutcomeIsNotRetried(t *testing.T) {
	s := newSMTP(t, false, map[string]string{"END": "DROP"})
	_, err := send(t, "smtp://"+s.addr+"?tls=none&from=a@example.com", invoice())
	if c, r, _ := reason(err); c != codes.Unavailable || r != ReasonOutcomeUnknown || s.conns != 1 {
		t.Fatalf("%v conns=%d", err, s.conns)
	}
}

func TestSMTPUnreachableIsRetried(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err := send(t, "smtp://"+addr+"?tls=none&from=a@example.com", invoice())
	if c, r, meta := reason(err); c != codes.Unavailable || r != ReasonTransport || meta["attempts"] != "3" {
		t.Fatalf("%v %v", err, meta)
	}
}

func TestUnconfigured(t *testing.T) {
	_, err := send(t, "", invoice())
	c, r, meta := reason(err)
	if c != codes.FailedPrecondition || r != ReasonUnconfigured || meta["env"] != "W17_CLIENT_MAIL_DSN" ||
		!strings.Contains(err.Error(), "W17_CLIENT_MAIL_DSN") {
		t.Fatalf("%v", err)
	}
}

func TestBadDSNFailsTheBoot(t *testing.T) {
	conn := inprocgrpc.New()
	err := RegisterFromEnv(conn, mailService(t), "mail", "W17_CLIENT_MAIL_DSN",
		func(string) (string, bool) { return "smtp://u:secret@h?tls=none", true })
	if err == nil || !strings.Contains(err.Error(), "W17_CLIENT_MAIL_DSN") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v", err)
	}
}

type apiCall struct {
	path   string
	header http.Header
	body   []byte
}

func api(t *testing.T, statuses ...int) (*httptest.Server, *[]apiCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []apiCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(calls)
		calls = append(calls, apiCall{r.URL.Path, r.Header.Clone(), b})
		mu.Unlock()
		st := 200
		if n < len(statuses) {
			st = statuses[n]
		}
		if st == 503 {
			w.Header().Set("Retry-After", "2")
		}
		w.Header().Set("X-Message-Id", "sg-1")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"MessageID":"pm-1","id":"id-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestPostmark(t *testing.T) {
	srv, calls := api(t, 503)
	r, err := send(t, "postmark://tok@?from=a@example.com&stream=outbound&endpoint="+srv.URL, invoice())
	if err != nil || r.GetProviderId() != "pm-1" || r.GetAttempts() != 2 || r.GetMessageId() != "" {
		t.Fatalf("%v %v", r, err)
	}
	c := (*calls)[1]
	var b map[string]any
	_ = json.Unmarshal(c.body, &b)
	if c.path != "/email" || c.header.Get("X-Postmark-Server-Token") != "tok" || b["Bcc"] != "archive@example.com" ||
		b["MessageStream"] != "outbound" || len(b["Attachments"].([]any)) != 3 {
		t.Fatalf("%s %v %v", c.path, c.header, b)
	}
}

func TestAPIAmbiguousFailureIsNotRetried(t *testing.T) {
	srv, calls := api(t, 500, 500, 500)
	_, err := send(t, "sendgrid://k@?from=a@example.com&endpoint="+srv.URL, invoice())
	if c, r, _ := reason(err); c != codes.Unavailable || r != "UPSTREAM_HTTP_500" || len(*calls) != 1 {
		t.Fatalf("%v calls=%d", err, len(*calls))
	}
	srv, calls = api(t, 422)
	_, err = send(t, "postmark://t@?from=a@example.com&endpoint="+srv.URL, invoice())
	if c, _, _ := reason(err); c != codes.InvalidArgument || len(*calls) != 1 {
		t.Fatalf("%v", err)
	}
}

func TestResendRetriesWithOneKey(t *testing.T) {
	srv, calls := api(t, 500)
	r, err := send(t, "resend://k@?from=a@example.com&endpoint="+srv.URL, invoice())
	if err != nil || r.GetProviderId() != "id-1" || len(*calls) != 2 {
		t.Fatalf("%v %v", r, err)
	}
	k1, k2 := (*calls)[0].header.Get("Idempotency-Key"), (*calls)[1].header.Get("Idempotency-Key")
	if k1 == "" || k1 != k2 || (*calls)[0].header.Get("Authorization") != "Bearer k" {
		t.Fatalf("keys %q %q", k1, k2)
	}
}

func TestSendGrid(t *testing.T) {
	srv, calls := api(t, 202)
	r, err := send(t, "sendgrid://k@?from=Faktury+<a@example.com>&endpoint="+srv.URL, invoice())
	if err != nil || r.GetProviderId() != "sg-1" {
		t.Fatalf("%v %v", r, err)
	}
	var b struct {
		From    sgAddr                `json:"from"`
		Content []map[string]string   `json:"content"`
		P       []map[string][]sgAddr `json:"personalizations"`
	}
	_ = json.Unmarshal((*calls)[0].body, &b)
	if b.From.Name != "Faktury" || b.Content[0]["type"] != "text/plain" || b.P[0]["bcc"][0].Email != "archive@example.com" {
		t.Fatalf("%s", (*calls)[0].body)
	}
}

func TestMailgunSendsTheMIME(t *testing.T) {
	srv, calls := api(t)
	r, err := send(t, "mailgun://key@mg.example.com?from=a@example.com&endpoint="+srv.URL, invoice())
	if err != nil || r.GetProviderId() != "id-1" || r.GetMessageId() == "" {
		t.Fatalf("%v %v", r, err)
	}
	c := (*calls)[0]
	if c.path != "/v3/mg.example.com/messages.mime" {
		t.Fatal(c.path)
	}
	if u, p, _ := (&http.Request{Header: c.header}).BasicAuth(); u != "api" || p != "key" {
		t.Fatalf("auth %q %q", u, p)
	}
	_, params, _ := mime.ParseMediaType(c.header.Get("Content-Type"))
	form, err := multipart.NewReader(bytes.NewReader(c.body), params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(form.Value["to"]) != 2 || form.Value["to"][1] != "archive@example.com" {
		t.Fatalf("to %v", form.Value["to"])
	}
	f, _ := form.File["message"][0].Open()
	raw, _ := io.ReadAll(f)
	if h, _, _ := readParts(t, raw); h.Get("Message-Id") != r.GetMessageId() || h.Get("Bcc") != "" {
		t.Fatalf("mime %v", h)
	}
}
