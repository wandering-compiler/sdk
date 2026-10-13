package dpopclient

import (
	"context"
	"errors"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// Credentials attaches DPoP to gRPC calls: it is the
// credentials.PerRPCCredentials that adds `authorization: DPoP <token>` and
// a fresh `dpop` proof to every call, and it provides the interceptors that
// read `dpop-nonce` from response headers and trailers.
//
// The proof of a call has htm POST and htu Origin + the full method name
// ("/package.Service/Method"). As with [Transport], a call gets DPoP only
// when Token returns a token or its context is marked with [MintContext];
// any other call carries no DPoP metadata.
//
// Unary calls refused with codes.Unauthenticated, a `www-authenticate`
// trailer holding `use_dpop_nonce` and a new `dpop-nonce` are retried once.
// Streaming calls are not retried — a stream's refusal surfaces on its first
// receive, after the caller may have sent — but the interceptor still learns
// the nonce, so the caller's next attempt carries it.
//
// Use [Credentials.DialOptions] to install the credentials and both
// interceptors; credentials alone attach proofs but never learn a nonce.
type Credentials struct {
	// Signer holds the device key. Required.
	Signer dpop.Signer
	// Origin is the public origin the server is configured with,
	// scheme://host[:port]. Required.
	Origin string
	// Token supplies the access token; nil means no token.
	Token TokenFunc
	// AllowInsecure lets the credentials travel over a connection without
	// transport security (tests, in-process connections). Off by default:
	// gRPC then refuses to send them in clear text.
	AllowInsecure bool

	mu    sync.Mutex
	nonce string
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c *Credentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	if c.Signer == nil {
		return nil, errors.New("dpopclient: Credentials has no Signer")
	}
	ri, ok := credentials.RequestInfoFromContext(ctx)
	if !ok || ri.Method == "" {
		return nil, errors.New("dpopclient: no gRPC method in the call context")
	}
	method := ri.Method
	if !strings.HasPrefix(method, "/") {
		method = "/" + method
	}
	token := ""
	if c.Token != nil {
		var err error
		if token, err = c.Token(ctx); err != nil {
			return nil, err
		}
	}
	if token == "" && !IsMint(ctx) {
		return map[string]string{}, nil
	}
	proof, err := dpop.NewProof(c.Signer, "POST", strings.TrimSuffix(c.Origin, "/")+method, token, c.currentNonce())
	if err != nil {
		return nil, err
	}
	md := map[string]string{dpop.MetadataProof: proof}
	if token != "" {
		md[dpop.MetadataAuthorization] = dpop.AuthScheme + " " + token
	}
	return md, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (c *Credentials) RequireTransportSecurity() bool { return !c.AllowInsecure }

func (c *Credentials) currentNonce() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonce
}

// learn stores the last nonce in mds and returns it ("" when none).
func (c *Credentials) learn(mds ...metadata.MD) string {
	n := ""
	for _, md := range mds {
		if v := md.Get(dpop.MetadataNonce); len(v) > 0 && v[len(v)-1] != "" {
			n = v[len(v)-1]
		}
	}
	if n == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nonce = n
	return n
}

// UnaryClientInterceptor learns nonces and retries a `use_dpop_nonce`
// refusal once. A DEVICE_REVOKED or SESSION_EXPIRED refusal comes back as a
// [*RefusedError].
func (c *Credentials) UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var hdr, trl metadata.MD
		call := make([]grpc.CallOption, 0, len(opts)+2)
		call = append(call, opts...)
		call = append(call, grpc.Header(&hdr), grpc.Trailer(&trl))
		used := c.currentNonce()
		err := invoker(ctx, method, req, reply, cc, call...)
		fresh := c.learn(hdr, trl)
		if err != nil && fresh != "" && fresh != used && status.Code(err) == codes.Unauthenticated &&
			dpop.IsUseNonceChallenge(trl.Get(dpop.MetadataWWWAuthenticate)) {
			hdr, trl = nil, nil
			err = invoker(ctx, method, req, reply, cc, call...)
			c.learn(hdr, trl)
		}
		return refusal(err)
	}
}

// StreamClientInterceptor learns nonces from a stream's header and trailer.
// It does not retry; see [Credentials].
func (c *Credentials) StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		s, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			return nil, refusal(err)
		}
		return &nonceStream{ClientStream: s, c: c}, nil
	}
}

// DialOptions returns the options that install the credentials and both
// interceptors.
func (c *Credentials) DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(c),
		grpc.WithChainUnaryInterceptor(c.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(c.StreamClientInterceptor()),
	}
}

type nonceStream struct {
	grpc.ClientStream
	c    *Credentials
	once sync.Once
}

func (s *nonceStream) Header() (metadata.MD, error) {
	md, err := s.ClientStream.Header()
	s.c.learn(md)
	return md, err
}

func (s *nonceStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.once.Do(func() {
			if md, herr := s.ClientStream.Header(); herr == nil {
				s.c.learn(md, s.Trailer())
			} else {
				s.c.learn(s.Trailer())
			}
		})
		return refusal(err)
	}
	return nil
}
