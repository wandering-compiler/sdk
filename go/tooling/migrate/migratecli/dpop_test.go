package migratecli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// dpopConsole is a console's fetch surface behind the SDK's own DPoP
// verifier, nonces on: a call with `authorization: DPoP` must carry a proof
// for the configured origin + the method, signed by the bound key, with the
// current nonce — the first call of a process never has one and is answered
// with the use_dpop_nonce challenge. A Bearer call is refused, as a console
// that binds devices refuses it.
type dpopConsole struct {
	applyfetchpb.UnimplementedMigrationFetchServer
	v       *dpop.Verifier
	binding string
	refuse  string // a w17.ErrorDetail code to refuse an otherwise valid call with

	mu    sync.Mutex
	calls []metadata.MD
	errs  []error
}

func (c *dpopConsole) check(ctx context.Context, method string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	c.mu.Lock()
	c.calls = append(c.calls, md)
	c.mu.Unlock()
	auth := md.Get(dpop.MetadataAuthorization)
	if len(auth) != 1 {
		return status.Error(codes.Unauthenticated, "no token")
	}
	tok, ok := dpop.AccessToken(auth[0])
	if !ok {
		return status.Error(codes.Unauthenticated, "token is not DPoP-bound")
	}
	_, err := c.v.Verify(ctx, dpop.Request{
		Method: "POST", Path: method, Proofs: md.Get(dpop.MetadataProof), AccessToken: tok, Binding: c.binding,
	})
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
	if err != nil {
		_ = grpc.SetTrailer(ctx, dpop.ChallengeMetadata(err))
		return status.Error(codes.Unauthenticated, err.Error())
	}
	if c.refuse != "" {
		st, _ := status.New(codes.Unauthenticated, "refused").WithDetails(&w17pb.ErrorDetail{Code: c.refuse, Message: "refused"})
		return st.Err()
	}
	return nil
}

func (c *dpopConsole) FetchMigrations(context.Context, *applyfetchpb.FetchMigrationsRequest) (*applyfetchpb.FetchMigrationsResponse, error) {
	return &applyfetchpb.FetchMigrationsResponse{}, nil
}

func (c *dpopConsole) seen() ([]metadata.MD, []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]metadata.MD(nil), c.calls...), append([]error(nil), c.errs...)
}

// serveDPoP listens on loopback in clear text (the grpc:// form) and returns
// the console plus its host:port. origin "" means the one migratecli derives
// for that address.
func serveDPoP(t *testing.T, k *dpop.Key, origin string) (*dpopConsole, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if origin == "" {
		origin = "https://" + addr
	}
	nonces, err := dpop.NewHMACNonces([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := dpop.NewVerifier(dpop.Config{Origin: origin, Nonces: nonces})
	if err != nil {
		t.Fatal(err)
	}
	c := &dpopConsole{v: v, binding: k.Thumbprint()}
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if err := c.check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return h(ctx, req)
	}))
	applyfetchpb.RegisterMigrationFetchServer(srv, c)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return c, addr
}

func writeKey(t *testing.T, kt dpop.KeyType, mode os.FileMode) (*dpop.Key, string) {
	t.Helper()
	k, err := dpop.GenerateKey(kt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deploy.key")
	if err := k.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return k, path
}

// With W17_DPOP_KEY unset — or set to "" — the token goes as a plain bearer
// and no proof rides along: what a console that binds no devices requires.
func TestFetch_NoDPoPKeySendsPlainBearer(t *testing.T) {
	for _, env := range []map[string]string{
		{envToken: "tok"},
		{envToken: "tok", envDPoPKey: ""},
	} {
		f := &fakeFetch{}
		addr := serveFetch(t, f, nil)
		env[envConsoleAddr] = "grpc://" + addr
		if err := fetchWithin(t, env); err != nil {
			t.Fatalf("%v: %v", env, err)
		}
		if got := f.calls(); len(got) != 1 || got[0] != "Bearer tok" {
			t.Fatalf("%v: calls = %v, want one plain bearer", env, got)
		}
	}
}

// With W17_DPOP_KEY set, every call carries `DPoP <token>` and a proof the
// SDK's verifier accepts for the token, the key it is bound to, and the
// origin derived from the address. The first call has no nonce, is
// challenged, and is retried once with it; the org header still rides.
func TestFetch_DPoPKeySignsTheCallAndAnswersTheNonceChallenge(t *testing.T) {
	for _, kt := range []dpop.KeyType{dpop.KeyP256, dpop.KeyEd25519} {
		t.Run(string(kt), func(t *testing.T) {
			k, path := writeKey(t, kt, 0o600)
			c, addr := serveDPoP(t, k, "")
			err := fetchWithin(t, map[string]string{
				envConsoleAddr: "grpc://" + addr, envToken: "tok", envDPoPKey: path, envConsoleOrg: "acme",
			})
			if err != nil {
				t.Fatalf("signed fetch: %v", err)
			}
			calls, errs := c.seen()
			if len(calls) != 2 {
				t.Fatalf("calls = %d, want 2 (the nonce challenge, then the retry)", len(calls))
			}
			if errs[0] == nil || !strings.Contains(errs[0].Error(), "nonce") {
				t.Fatalf("first call: want the nonce challenge, got %v", errs[0])
			}
			if errs[1] != nil {
				t.Fatalf("retry was refused: %v", errs[1])
			}
			for i, md := range calls {
				if got := md.Get("authorization"); len(got) != 1 || got[0] != "DPoP tok" {
					t.Errorf("call %d authorization = %v, want DPoP tok", i, got)
				}
				if got := md.Get("w17-org"); len(got) != 1 || got[0] != "acme" {
					t.Errorf("call %d w17-org = %v, want acme", i, got)
				}
			}
		})
	}
}

// A console reached by a name its configuration does not use is signed for
// with W17_CONSOLE_DPOP_ORIGIN; without it the proof's htu does not match.
func TestFetch_DPoPOriginOverride(t *testing.T) {
	k, path := writeKey(t, dpop.KeyP256, 0o600)
	c, addr := serveDPoP(t, k, "https://api.example.com:50051")
	env := map[string]string{envConsoleAddr: "grpc://" + addr, envToken: "tok", envDPoPKey: path}
	if err := fetchWithin(t, env); err == nil {
		t.Fatal("a proof for the dialed origin was accepted by a console configured with another")
	}
	env[envDPoPOrigin] = "https://api.example.com:50051/"
	if err := fetchWithin(t, env); err != nil {
		t.Fatalf("with %s: %v", envDPoPOrigin, err)
	}
	if _, errs := c.seen(); errs[len(errs)-1] != nil {
		t.Fatalf("last call refused: %v", errs[len(errs)-1])
	}
}

// A key that cannot be used is refused before anything is dialed, with a
// sentence naming the file: missing, writable by others, or not a key. A
// key that is only READABLE by others (a mounted secret) is used.
func TestFetch_UnusableDPoPKeyIsAClearError(t *testing.T) {
	f := &fakeFetch{}
	addr := serveFetch(t, f, nil)
	try := func(path string) error {
		return fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + addr, envToken: "tok", envDPoPKey: path})
	}

	missing := filepath.Join(t.TempDir(), "nope.key")
	if err := try(missing); err == nil || !strings.Contains(err.Error(), envDPoPKey) || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing key: %v", err)
	}

	garbage := filepath.Join(t.TempDir(), "garbage.key")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := try(garbage); err == nil || !strings.Contains(err.Error(), "PKCS #8") {
		t.Errorf("garbage key: %v", err)
	}

	if runtime.GOOS != "windows" {
		for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
			_, path := writeKey(t, dpop.KeyP256, mode)
			if err := try(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("mode %04o: %v", mode, err)
			}
		}
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("an unusable key still reached the console: %v", got)
	}

	if runtime.GOOS != "windows" {
		k, path := writeKey(t, dpop.KeyP256, 0o444)
		_, daddr := serveDPoP(t, k, "")
		if err := fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + daddr, envToken: "tok", envDPoPKey: path}); err != nil {
			t.Fatalf("a read-only mounted secret (0444) was refused: %v", err)
		}
	}
}

// The refusal says which half to change: an unsigned call is pointed at
// W17_DPOP_KEY, a signed one at the token-for-this-key rule and at unsetting
// the key for a console that binds no devices, a revoked key at a new one.
func TestFetch_RefusalNamesTheDeviceKey(t *testing.T) {
	f := &fakeFetch{err: status.Error(codes.Unauthenticated, "token is not DPoP-bound")}
	addr := serveFetch(t, f, nil)
	err := fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + addr, envToken: "tok"})
	if err == nil || !strings.Contains(err.Error(), "set W17_DPOP_KEY") || !strings.Contains(err.Error(), "W17_TOKEN") {
		t.Errorf("unsigned refusal:\n%v", err)
	}

	_, path := writeKey(t, dpop.KeyP256, 0o600)
	err = fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + addr, envToken: "tok", envDPoPKey: path})
	if err == nil || !strings.Contains(err.Error(), "unset W17_DPOP_KEY") {
		t.Errorf("signed refusal:\n%v", err)
	}

	k, path := writeKey(t, dpop.KeyP256, 0o600)
	c, daddr := serveDPoP(t, k, "")
	c.refuse = "DEVICE_REVOKED"
	err = fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + daddr, envToken: "tok", envDPoPKey: path})
	if err == nil || !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "w17ctl key public") {
		t.Errorf("revoked refusal:\n%v", err)
	}
}

// The origin is w17ctl's derivation, so one address signs for one origin in
// both tools.
func TestDPoPOrigin(t *testing.T) {
	for addr, want := range map[string]string{
		"grpcs://API.w17.app:50051":       "https://api.w17.app:50051",
		"api.w17.app:50051":               "https://api.w17.app:50051",
		"grpc://console:50051":            "https://console:50051",
		"grpcs://api.w17.app:443":         "https://api.w17.app",
		"dns:///api.w17.app:50051":        "https://api.w17.app:50051",
		"dns://8.8.8.8/api.w17.app:50051": "https://api.w17.app:50051",
		"[::1]:50051":                     "https://[::1]:50051",
	} {
		if got := dpopOrigin(addr, fetchEnv(nil)); got != want {
			t.Errorf("dpopOrigin(%q) = %q, want %q", addr, got, want)
		}
	}
	if got := dpopOrigin("console:50051", fetchEnv(map[string]string{envDPoPOrigin: " https://api.w17.app:50051/ "})); got != "https://api.w17.app:50051" {
		t.Errorf("override: %q", got)
	}
}
