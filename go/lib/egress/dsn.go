package egress

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Auth is how a client authenticates to its upstream.
type Auth string

const (
	AuthNone   Auth = "none"
	AuthBearer Auth = "bearer"
	AuthBasic  Auth = "basic"
	AuthAPIKey Auth = "api_key"
	// AuthOAuth2CC is the client-credentials grant. It is parsed, so a DSN
	// written for it is recognised, and refused until the token cache that
	// runs it ships.
	AuthOAuth2CC Auth = "oauth2_cc"
)

// DSN is a client's connection string, parsed:
//
//	https://api.example.com/v1?auth=bearer&token=…&timeout=10s&max_attempts=3
//
// The base URL is everything but the query; the query holds the connection
// parameters. The whole string is a secret (it carries the credential), so
// nothing here prints it: [DSN.String] masks it.
type DSN struct {
	Base *url.URL // scheme, host, optional port and path prefix; no query

	Auth Auth
	// Token is the bearer token.
	Token string
	// User and Password are basic auth — from the parameters or the URL's
	// userinfo.
	User, Password string
	// Key is the API key, sent in the header (default) or the query
	// parameter KeyName.
	Key, KeyName string
	KeyInQuery   bool

	// Timeout bounds one attempt; zero = the default.
	Timeout time.Duration
	// MaxAttempts bounds the attempts of a retryable call; zero = the default.
	MaxAttempts int
	// Proxy is a static egress proxy; nil = none (not even the environment's).
	Proxy *url.URL
}

// ParseDSN reads a DSN. An unknown parameter, or one that does not belong to
// the auth scheme, is an error: a misspelt `tokn=` must not quietly send
// requests without a credential.
func ParseDSN(s string) (*DSN, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("egress DSN: not a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("egress DSN: scheme %q, want https (or http)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress DSN: no host")
	}
	q := u.Query()
	d := &DSN{Auth: AuthNone}
	if a := q.Get("auth"); a != "" {
		d.Auth = Auth(a)
	}
	allowed := map[string]bool{"auth": true, "timeout": true, "max_attempts": true, "proxy": true}
	switch d.Auth {
	case AuthNone:
	case AuthBearer:
		allowed["token"] = true
		if d.Token = q.Get("token"); d.Token == "" {
			return nil, fmt.Errorf("egress DSN: auth=bearer needs token=")
		}
	case AuthBasic:
		allowed["user"], allowed["password"] = true, true
		d.User, d.Password = q.Get("user"), q.Get("password")
		if u.User != nil {
			if d.User != "" {
				return nil, fmt.Errorf("egress DSN: basic credentials in both the URL and user=")
			}
			d.User = u.User.Username()
			d.Password, _ = u.User.Password()
		}
		if d.User == "" {
			return nil, fmt.Errorf("egress DSN: auth=basic needs user= (or userinfo in the URL)")
		}
	case AuthAPIKey:
		allowed["key"], allowed["in"], allowed["key_name"] = true, true, true
		if d.Key = q.Get("key"); d.Key == "" {
			return nil, fmt.Errorf("egress DSN: auth=api_key needs key=")
		}
		switch in := q.Get("in"); in {
		case "", "header":
		case "query":
			d.KeyInQuery = true
		default:
			return nil, fmt.Errorf("egress DSN: in=%q, want header or query", in)
		}
		if d.KeyName = q.Get("key_name"); d.KeyName == "" {
			d.KeyName = "X-API-Key"
		}
	case AuthOAuth2CC:
		return nil, fmt.Errorf("egress DSN: auth=oauth2_cc is not supported yet — the client-credentials grant ships in its own release")
	default:
		return nil, fmt.Errorf("egress DSN: auth=%q, want none, bearer, basic or api_key", d.Auth)
	}
	if u.User != nil && d.Auth != AuthBasic {
		return nil, fmt.Errorf("egress DSN: userinfo in the URL is basic auth — set auth=basic")
	}
	for k := range q {
		if !allowed[k] {
			return nil, fmt.Errorf("egress DSN: unknown parameter %q for auth=%s", k, d.Auth)
		}
	}
	if t := q.Get("timeout"); t != "" {
		if d.Timeout, err = time.ParseDuration(t); err != nil || d.Timeout <= 0 {
			return nil, fmt.Errorf("egress DSN: timeout=%q is not a positive Go duration", t)
		}
	}
	if m := q.Get("max_attempts"); m != "" {
		if d.MaxAttempts, err = strconv.Atoi(m); err != nil || d.MaxAttempts < 1 {
			return nil, fmt.Errorf("egress DSN: max_attempts=%q is not a positive integer", m)
		}
	}
	if p := q.Get("proxy"); p != "" {
		if d.Proxy, err = url.Parse(p); err != nil || d.Proxy.Host == "" {
			return nil, fmt.Errorf("egress DSN: proxy is not a URL")
		}
	}
	base := *u
	base.User, base.RawQuery, base.ForceQuery, base.Fragment = nil, "", false, ""
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawPath = ""
	d.Base = &base
	return d, nil
}

// String is the DSN with its credentials masked: the base URL and the auth
// scheme. It is what logs, spans and errors may print.
func (d *DSN) String() string {
	return d.Base.String() + " (auth=" + string(d.Auth) + ")"
}
