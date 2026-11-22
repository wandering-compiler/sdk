package egress

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/grpc"
)

// Credential is a credential that comes with the call — a customer's own
// token for their account at the upstream — for a client declaring
// per_call_credential. Make one with [Bearer], [Basic] or [APIKey]; it must
// match the DSN's auth scheme. Nothing prints it.
type Credential struct {
	auth                       Auth
	token, user, password, key string
}

// Bearer is a per-call bearer token.
func Bearer(token string) Credential { return Credential{auth: AuthBearer, token: token} }

// Basic is a per-call user and password.
func Basic(user, password string) Credential {
	return Credential{auth: AuthBasic, user: user, password: password}
}

// APIKey is a per-call API key, sent where the DSN's in= / key_name= say.
func APIKey(key string) Credential { return Credential{auth: AuthAPIKey, key: key} }

// String masks the credential: a format verb never prints it.
func (c Credential) String() string { return "egress.Credential(" + string(c.auth) + ", masked)" }

// GoString masks it for %#v too.
func (c Credential) GoString() string { return c.String() }

func (c Credential) empty() bool {
	switch c.auth {
	case AuthBearer:
		return c.token == ""
	case AuthBasic:
		return c.user == ""
	case AuthAPIKey:
		return c.key == ""
	}
	return true
}

// Destination is the base URL a call goes to, for a client declaring
// per_call_destination: `https://hooks.example.com/w17` — scheme, host,
// optional port and path prefix, nothing else. Make one with [To].
type Destination struct{ base *url.URL }

// To checks a destination chosen at run time (a customer's webhook URL).
// It refuses a URL that is not https or http, names no host, carries a
// credential, a query or a fragment. Whether the HOST may be reached is
// decided when it is dialled, against the client's DSN policy.
func To(raw string) (Destination, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Destination{}, fmt.Errorf("egress destination: not a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Destination{}, fmt.Errorf("egress destination: scheme %q, want https (or http)", u.Scheme)
	}
	if u.Hostname() == "" {
		return Destination{}, fmt.Errorf("egress destination: no host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Destination{}, fmt.Errorf("egress destination: a credential, query or fragment in the URL — it is a base URL; the call brings the rest")
	}
	b := *u
	b.Path = strings.TrimSuffix(b.Path, "/")
	b.RawPath = ""
	return Destination{base: &b}, nil
}

// String is the destination's URL (it carries no credential).
func (d Destination) String() string {
	if d.base == nil {
		return ""
	}
	return d.base.String()
}

type callKey struct{}

type callValues struct {
	dest *Destination
	cred *Credential
}

// Bind is the connection a per-call client's getter calls through: every
// call on it carries dest and cred (either may be nil) to the client served
// in-process. The project's generated getters call it; a caller does not.
//
// They travel in the call's context, not its metadata, so they never leave
// the process and no interceptor logs them. A client served by hand on a
// connection of its own does not receive them, and refuses the call.
func Bind(cc grpc.ClientConnInterface, dest *Destination, cred *Credential) grpc.ClientConnInterface {
	return bound{cc: cc, v: callValues{dest: dest, cred: cred}}
}

type bound struct {
	cc grpc.ClientConnInterface
	v  callValues
}

func (b bound) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return b.cc.Invoke(context.WithValue(ctx, callKey{}, b.v), method, args, reply, opts...)
}

func (b bound) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return b.cc.NewStream(context.WithValue(ctx, callKey{}, b.v), desc, method, opts...)
}

func callValuesOf(ctx context.Context) callValues {
	v, _ := ctx.Value(callKey{}).(callValues)
	return v
}
