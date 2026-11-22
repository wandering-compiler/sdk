package mail

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Provider is how a DSN sends.
type Provider string

const (
	SMTP     Provider = "smtp"  // STARTTLS on submission (587), or plain with tls=none
	SMTPS    Provider = "smtps" // implicit TLS (465)
	Postmark Provider = "postmark"
	SendGrid Provider = "sendgrid"
	Mailgun  Provider = "mailgun"
	Resend   Provider = "resend"
)

// The call policy's defaults. A DSN overrides the attempt timeout and the
// attempt count; the total bound is fixed.
const (
	DefaultTimeout     = 30 * time.Second
	DefaultMaxAttempts = 3
	// TotalTimeout bounds the whole call, every attempt and back-off
	// included.
	TotalTimeout = 90 * time.Second
	// MaxMessageBytes caps a message: the body and every attachment, before
	// encoding. Common providers accept 25 MB encoded; base64 adds a third.
	MaxMessageBytes = 20 << 20
)

// DSN is a parsed W17_CLIENT_<NAME>_DSN of a mail client. String never
// prints the credential.
type DSN struct {
	Provider Provider
	// Host and Port: the SMTP server; for Mailgun, Host is the sending
	// domain.
	Host string
	Port string
	// User and Password: the SMTP login (both empty = no AUTH). For an API,
	// Password is its key or token.
	User, Password string
	// PlainText: smtp:// without STARTTLS (tls=none) — a local catcher
	// only (Mailpit, MailHog); never with a login over the network.
	PlainText bool
	// From is the default sender, a message without its own.
	From string
	// Endpoint replaces the API's base URL (an EU region, a test server).
	Endpoint string
	// Stream is Postmark's message stream (`outbound` when empty).
	Stream string
	// Region is Mailgun's (`us` or `eu`).
	Region string
	// HELO is the name the SMTP client greets with; empty = localhost.
	HELO string

	Timeout     time.Duration
	MaxAttempts int
}

// ParseDSN parses a mail client's DSN. The error never quotes the DSN: it
// carries a credential.
func ParseDSN(s string) (*DSN, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("mail DSN: not a URL (smtps://user:password@host:465, postmark://<token>@, …)")
	}
	d := &DSN{Provider: Provider(strings.ToLower(u.Scheme))}
	q := u.Query()
	pass, _ := u.User.Password()
	switch d.Provider {
	case SMTP, SMTPS:
		d.Host = u.Hostname()
		if d.Host == "" {
			return nil, fmt.Errorf("mail DSN: %s:// names no host", d.Provider)
		}
		d.Port = u.Port()
		if d.Port == "" {
			d.Port = map[Provider]string{SMTP: "587", SMTPS: "465"}[d.Provider]
		}
		d.User, d.Password = u.User.Username(), pass
		if (d.User == "") != (d.Password == "") {
			return nil, fmt.Errorf("mail DSN: an SMTP login is user and password, both or neither")
		}
		switch t := q.Get("tls"); t {
		case "", "starttls":
		case "none":
			if d.Provider == SMTPS {
				return nil, fmt.Errorf("mail DSN: smtps:// is TLS; tls=none is for smtp://")
			}
			if d.User != "" && !loopback(d.Host) {
				return nil, fmt.Errorf("mail DSN: tls=none would send the SMTP login in clear text to %s — use STARTTLS or smtps://", d.Host)
			}
			d.PlainText = true
		default:
			return nil, fmt.Errorf("mail DSN: tls=%q (want starttls or none)", t)
		}
		d.HELO = q.Get("helo")
	case Postmark, SendGrid, Resend, Mailgun:
		key := u.User.Username()
		if pass != "" {
			key = pass
		}
		if key == "" {
			return nil, fmt.Errorf("mail DSN: %s:// carries no API key (%s://<key>@)", d.Provider, d.Provider)
		}
		d.Password = key
		d.Host = u.Hostname()
		if d.Provider == Mailgun && d.Host == "" {
			return nil, fmt.Errorf("mail DSN: mailgun:// names its sending domain as the host (mailgun://<key>@mg.example.com)")
		}
		if d.Provider != Mailgun && d.Host != "" {
			return nil, fmt.Errorf("mail DSN: %s:// takes no host — the API is fixed (endpoint= replaces it)", d.Provider)
		}
		d.Stream = q.Get("stream")
		if d.Stream != "" && d.Provider != Postmark {
			return nil, fmt.Errorf("mail DSN: stream= is Postmark's")
		}
		switch d.Region = q.Get("region"); {
		case d.Region != "" && d.Provider != Mailgun:
			return nil, fmt.Errorf("mail DSN: region= is Mailgun's")
		case d.Region != "" && d.Region != "us" && d.Region != "eu":
			return nil, fmt.Errorf("mail DSN: region=%q (want us or eu)", d.Region)
		}
		if e := q.Get("endpoint"); e != "" {
			eu, err := url.Parse(e)
			if err != nil || eu.Host == "" || (eu.Scheme != "https" && (eu.Scheme != "http" || !loopback(eu.Hostname()))) {
				return nil, fmt.Errorf("mail DSN: endpoint= is an https:// URL (http:// only to a loopback address)")
			}
			d.Endpoint = strings.TrimSuffix(e, "/")
		}
	case "":
		return nil, fmt.Errorf("mail DSN: no scheme (smtps://, smtp://, postmark://, sendgrid://, mailgun://, resend://)")
	default:
		return nil, fmt.Errorf("mail DSN: unknown scheme %q (smtps, smtp, postmark, sendgrid, mailgun, resend)", u.Scheme)
	}
	if f := q.Get("from"); f != "" {
		if _, err := parseAddress(f); err != nil {
			return nil, fmt.Errorf("mail DSN: from=: %w", err)
		}
		d.From = f
	}
	if t := q.Get("timeout"); t != "" {
		if d.Timeout, err = time.ParseDuration(t); err != nil || d.Timeout <= 0 {
			return nil, fmt.Errorf("mail DSN: timeout=%q is not a positive duration", t)
		}
		if d.Timeout >= TotalTimeout {
			return nil, fmt.Errorf("mail DSN: timeout=%s is not below the call's total deadline (%s)", d.Timeout, TotalTimeout)
		}
	}
	if m := q.Get("max_attempts"); m != "" {
		if d.MaxAttempts, err = strconv.Atoi(m); err != nil || d.MaxAttempts < 1 || d.MaxAttempts > 10 {
			return nil, fmt.Errorf("mail DSN: max_attempts=%q (want 1 to 10)", m)
		}
	}
	for k := range q {
		if !known[k] {
			return nil, fmt.Errorf("mail DSN: unknown parameter %q", k)
		}
	}
	return d, nil
}

var known = map[string]bool{"from": true, "timeout": true, "max_attempts": true, "tls": true, "helo": true,
	"endpoint": true, "stream": true, "region": true}

func (d *DSN) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultTimeout
}

func (d *DSN) maxAttempts() int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return DefaultMaxAttempts
}

// Server is where the messages go, for a span: the SMTP server, or the
// API's host. Never the credential.
func (d *DSN) Server() string {
	switch d.Provider {
	case SMTP, SMTPS:
		return net.JoinHostPort(d.Host, d.Port)
	}
	if d.Endpoint != "" {
		if u, err := url.Parse(d.Endpoint); err == nil {
			return u.Host
		}
	}
	if u, err := url.Parse(d.baseURL()); err == nil {
		return u.Host
	}
	return string(d.Provider)
}

// String is the DSN without its credential.
func (d *DSN) String() string {
	s := string(d.Provider) + "://"
	if d.Password != "" {
		s += "REDACTED@"
	}
	return s + d.Host
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
