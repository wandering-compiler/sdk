package migratecli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"runtime"
	"strings"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
)

// The device key a deployment signs its fetch with.
//
// A console whose auth binds devices (DPoP, RFC 9449) binds every token it
// issues to a key, and accepts the token only together with a fresh proof
// signed by that key. A machine account's token for a deployment is issued for
// the PUBLIC half of a key the deployment holds — `W17_DPOP_KEY=<file> w17ctl
// key public` makes the key and prints it — and the deployment then sets
// W17_DPOP_KEY to the same file beside W17_TOKEN. A token copied off the box
// is useless without the key file.
//
// The semantics are w17ctl's for an environment token, deliberately: one
// deploy configuration drives both, and a variable that meant something
// different in the two would fail in whichever half the operator did not test.
//
//   - W17_DPOP_KEY unset or empty: the token goes as `Bearer`, which is what a
//     console that binds no devices requires — it refuses the DPoP scheme.
//   - W17_DPOP_KEY set: `authorization: DPoP <token>` plus a `dpop` proof per
//     call, htu = the console's origin + the gRPC method, the console's nonce
//     learned and a stale-nonce refusal retried once (dpopclient).
//
// Nothing here guesses which kind of console it talks to: the operator's
// choice of variable is the answer, and the console's refusal of the wrong
// one is explained by fetchRefusal.

const (
	// envDPoPKey is the PATH to the device key: a PKCS #8 PEM holding a P-256
	// or Ed25519 private key. Same variable as w17ctl.
	envDPoPKey = "W17_DPOP_KEY"
	// envDPoPOrigin overrides the origin proofs are signed for. Same variable
	// as w17ctl; see dpopOrigin.
	envDPoPOrigin = "W17_CONSOLE_DPOP_ORIGIN"
)

// loadDeviceKey reads the key W17_DPOP_KEY names.
//
// It refuses a file that group or others can WRITE — a key someone else can
// replace binds nothing — but not one they can read: the key is typically a
// mounted secret, and the common mounts are readable beyond the owner (a swarm
// secret under /run/secrets is 0444, a Kubernetes Secret volume 0644) inside a
// container that runs one user anyway. That is w17ctl's rule for a key the
// operator names.
func loadDeviceKey(path string) (*dpop.Key, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf(
				"migrate: %s names %s, which does not exist\n"+
					"  fix: point %s at the device key the deploy token was issued for,\n"+
					"       or unset it to send W17_TOKEN as a plain bearer (a console that binds no devices)",
				envDPoPKey, path, envDPoPKey)
		}
		return nil, fmt.Errorf("migrate: device key %s: %w", path, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf(
			"migrate: device key %s is writable by other users (mode %04o) and is not used\n"+
				"  fix: chmod 600 %s",
			path, fi.Mode().Perm(), path)
	}
	k, err := dpop.LoadKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf(
			"migrate: device key %s: %w\n"+
				"  want: a PKCS #8 \"PRIVATE KEY\" PEM holding a P-256 or Ed25519 key (w17ctl key public writes one)",
			path, err)
	}
	return k, nil
}

// dpopOrigin is the origin proofs are signed for when addr is dialed: the
// proof's htu is this plus the gRPC method ("/pkg.Service/Method"), and the
// console compares it with the DPoP origin its configuration names.
//
// It is w17ctl's derivation, so one address yields one origin in both tools:
// https://host[:port], the host lowercased, the default port 443 left out, a
// readability scheme (grpcs://, grpc://) and a gRPC resolver prefix (dns:///)
// dropped. It is https even for a grpc:// dial — a sidecar or loopback proxy
// the operator chose to reach in clear text does not change the console's
// public origin; when the dialed name is not that origin, W17_CONSOLE_DPOP_ORIGIN
// names it.
func dpopOrigin(addr string, getenv func(string) string) string {
	if o := strings.TrimSpace(getenv(envDPoPOrigin)); o != "" {
		return strings.TrimSuffix(o, "/")
	}
	target := addr
	for _, s := range []string{"https", "grpcs", "http", "grpc"} {
		if rest, ok := strings.CutPrefix(target, s+"://"); ok {
			target = rest
			break
		}
	}
	if i := strings.Index(target, ":///"); i >= 0 {
		target = target[i+len(":///"):]
	} else if i := strings.Index(target, "://"); i >= 0 {
		// dns://authority/host:port — the authority is the resolver's, not
		// the console's.
		rest := target[i+len("://"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
		}
		target = rest
	}
	target = strings.TrimSuffix(target, "/")
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]"), ""
	}
	host = strings.ToLower(host)
	if port == "" || port == "443" {
		if strings.Contains(host, ":") {
			return "https://[" + host + "]"
		}
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

// fetchCredentials returns the dial options that authenticate the fetch: the
// plain bearer when W17_DPOP_KEY is unset, the DPoP credential and its nonce
// interceptor when it is set. A key that cannot be used is an error here,
// before anything is dialed — a fetch that went out unsigned would only be
// refused, with a reason that names the token rather than the key.
func fetchCredentials(addr string, getenv func(string) string) (opts []grpc.DialOption, signed bool, err error) {
	path := getenv(envDPoPKey)
	if path == "" {
		return []grpc.DialOption{grpc.WithPerRPCCredentials(bearerCreds{getenv: getenv})}, false, nil
	}
	k, err := loadDeviceKey(path)
	if err != nil {
		return nil, false, err
	}
	dc := &dpopclient.Credentials{
		Signer: k,
		Origin: dpopOrigin(addr, getenv),
		Token: func(context.Context) (string, error) {
			return getenv(envToken), nil
		},
		// As bearerCreds: a deliberate grpc:// dial still carries the
		// credential. TLS is the default for every other form (transportFor).
		AllowInsecure: true,
	}
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(dpopCreds{dc: dc, getenv: getenv}),
		grpc.WithChainUnaryInterceptor(dc.UnaryClientInterceptor()),
	}, true, nil
}

// dpopCreds is the DPoP credential plus the active-org header bearerCreds
// sends: the org is a property of the call, not of the token's scheme.
type dpopCreds struct {
	dc     *dpopclient.Credentials
	getenv func(string) string
}

func (d dpopCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	md, err := d.dc.GetRequestMetadata(ctx, uri...)
	if err != nil || len(md) == 0 {
		// No token: no proof and no org, exactly as bearerCreds sends
		// nothing — whether the fetch surface is gated is the console's call.
		return md, err
	}
	if org := d.getenv(envConsoleOrg); org != "" {
		md["w17-org"] = org
	}
	return md, nil
}

func (d dpopCreds) RequireTransportSecurity() bool { return d.dc.RequireTransportSecurity() }
