package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
)

// smtpSender sends over SMTP: implicit TLS (smtps), STARTTLS (smtp, always
// required — a server that does not offer it is refused, never fallen back
// from), or plain text to a local catcher (tls=none).
type smtpSender struct {
	client string
	dsn    *DSN
	tls    *tls.Config // nil = the system roots, ServerName = the host
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (*smtpSender) wantsMIME() bool { return true }

func (s *smtpSender) tlsConfig() *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.tls != nil {
		c = s.tls.Clone()
	}
	if c.ServerName == "" {
		c.ServerName = s.dsn.Host
	}
	return c
}

// send is one attempt. Its outcome says whether a retry is safe: anything
// before the end of DATA was not accepted, a 4xx reply never is; once the
// message's final dot is written and no reply came, it may have been.
func (s *smtpSender) send(ctx context.Context, m *message, raw []byte) (string, outcome) {
	addr := net.JoinHostPort(s.dsn.Host, s.dsn.Port)
	conn, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return "", notSent(transportFailure(s.client, "connect to "+addr+": "+transportText(err)))
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A context cancelled mid-conversation closes the connection: net/smtp
	// takes no context of its own.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stopWatch()

	if s.dsn.Provider == SMTPS {
		tc := tls.Client(conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return "", notSent(transportFailure(s.client, "TLS with "+addr+": "+transportText(err)))
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, s.dsn.Host)
	if err != nil {
		return "", notSent(s.replyError("greeting", err))
	}
	defer func() { _ = c.Close() }()
	helo := s.dsn.HELO
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return "", notSent(s.replyError("EHLO", err))
	}
	if s.dsn.Provider == SMTP && !s.dsn.PlainText {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return "", final(callError(codes.Internal, s.client, ReasonTLSUnavailable,
				addr+" does not offer STARTTLS; the message is not sent in clear text (smtps:// for implicit TLS, tls=none only for a local catcher)", nil))
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return "", notSent(transportFailure(s.client, "STARTTLS with "+addr+": "+transportText(err)))
		}
	}
	if s.dsn.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.dsn.User, s.dsn.Password, s.dsn.Host)); err != nil {
			return "", s.classify("AUTH", err)
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return "", s.classify("MAIL FROM", err)
	}
	for _, r := range m.recipients() {
		if err := c.Rcpt(r); err != nil {
			return "", s.classify("RCPT TO", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return "", s.classify("DATA", err)
	}
	if _, err := w.Write(raw); err != nil {
		return "", notSent(s.replyError("DATA", err))
	}
	// Close writes the final dot and reads the server's verdict: from here
	// on, a failure without a reply leaves the message's fate unknown.
	if err := w.Close(); err != nil {
		var tp *textproto.Error
		if errors.As(err, &tp) {
			return "", s.classify("end of DATA", err)
		}
		return "", final(callError(codes.Unavailable, s.client, ReasonOutcomeUnknown,
			"the connection ended before the server answered the message: it may have been accepted — not retried, to send no second copy",
			map[string]string{"cause": transportText(err)}))
	}
	_ = c.Quit()
	return "", sent()
}

// classify is an SMTP reply's outcome: 4xx is temporary (the message was
// not accepted, a retry is safe), 5xx is permanent.
func (s *smtpSender) classify(stage string, err error) outcome {
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 400 && tp.Code < 500 {
		return notSent(s.replyError(stage, err))
	}
	return final(s.replyError(stage, err))
}

func (s *smtpSender) replyError(stage string, err error) error {
	var tp *textproto.Error
	if !errors.As(err, &tp) {
		return transportFailure(s.client, stage+": "+transportText(err))
	}
	code := codes.Internal
	switch {
	case tp.Code >= 400 && tp.Code < 500:
		code = codes.Unavailable
	case tp.Code == 530 || tp.Code == 534 || tp.Code == 535:
		// Our login, never the caller's fault.
		code = codes.Internal
	case stage == "RCPT TO" || stage == "MAIL FROM" || stage == "end of DATA":
		// A mailbox refused, a sender not allowed, a message rejected for
		// its content: what the caller sent.
		code = codes.InvalidArgument
	}
	return callError(code, s.client, "UPSTREAM_SMTP_"+strconv.Itoa(tp.Code),
		stage+": the server answered "+strconv.Itoa(tp.Code),
		map[string]string{"smtp_code": strconv.Itoa(tp.Code), "reply": truncate([]byte(tp.Msg), maxErrorBody)})
}
