// Package healthcheck is the `health` subcommand every generated w17 server
// binary mounts: `<binary> health` asks each listener THIS binary opens
// whether it answers, and exits non-zero naming the one that did not.
//
// Why a subcommand: the images are distroless — no shell, no curl, no wget —
// so a container HEALTHCHECK has nothing to run but the binary itself.
// Without one, an orchestrator (Swarm `update_config.order: start-first`,
// compose `service_healthy`) treats a container as ready the moment its
// process starts, and routes traffic to a server that is still waiting on its
// database. The generated Dockerfile declares `HEALTHCHECK CMD ["/server",
// "health"]`, so a project gets the check without editing a compose file.
//
// It reads NO configuration of its own. Which listeners exist and where they
// bind is what the server resolved at startup from the same environment; the
// generated code hands this package the SAME functions the server calls, so
// a probe cannot be pointed at a port the server does not use. A second copy
// of "which env var, which default" would agree with the server until the day
// one of them changed.
//
// LIVENESS, not readiness. An HTTP listener answers GET [healthz.Path]; a gRPC
// listener answers grpc.health.v1 Check on the root service. Neither consults
// a database or a backend: a database outage must not restart every service
// that depends on it.
package healthcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wandering-compiler/sdk/go/core/healthz"
	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/binroots"
)

// Command is the argv[1] word the binary answers to. It is a reserved CLI
// root (tooling/migrate/binroots) so a project command cannot shadow it.
const Command = binroots.Health

// DefaultTimeout bounds the whole probe when --timeout is not given. Short on
// purpose: an orchestrator runs this every few seconds, and a listener that
// cannot answer a no-op in three seconds is not serving anybody either.
const DefaultTimeout = 3 * time.Second

type kind int

const (
	kindHTTP kind = iota
	kindGRPC
	kindPublicGRPC
)

// Probe is one listener to ask. Build it with [HTTP] or [GRPC].
type Probe struct {
	// Name labels the listener in the output ("rest", "mcp", "grpc", …).
	Name string
	// Addr is the address the server LISTENS on, exactly as it resolved it
	// (":8080", "0.0.0.0:50051", "10.0.0.5:9090"). The probe turns it into
	// a dial target itself; see target.
	Addr string
	kind kind
}

// HTTP probes an HTTP listener: GET [healthz.Path] must answer 200.
func HTTP(name, listenAddr string) Probe {
	return Probe{Name: name, Addr: listenAddr, kind: kindHTTP}
}

// GRPC probes a gRPC listener: grpc.health.v1 Check on the root service ("")
// must answer SERVING. Dialled with the stack-wide internal-TLS posture, the
// same one the server listens with (see dialOption).
func GRPC(name, listenAddr string) Probe {
	return Probe{Name: name, Addr: listenAddr, kind: kindGRPC}
}

// PublicGRPC probes a PUBLIC gRPC listener (the rpc gateway) the way [GRPC]
// does, but always in plaintext: the public edge terminates TLS at the
// operator's proxy and the listener itself never serves it, whatever the
// internal-mesh switch says. Probing it with the mesh posture would report a
// serving gateway as down on every stack that encrypts its internal hops.
func PublicGRPC(name, listenAddr string) Probe {
	return Probe{Name: name, Addr: listenAddr, kind: kindPublicGRPC}
}

// Options is what the generated binary knows and this package does not.
type Options struct {
	// Probes are the listeners this binary opens. Empty is an error at run
	// time, not a pass: a binary with nothing to ask has nothing to report
	// healthy, and saying "ok" would certify a check that never ran.
	Probes []Probe

	// Out is where the per-listener lines go. Defaults to os.Stdout, which
	// is what `docker inspect` shows as the health log.
	Out io.Writer

	// Getenv reads the internal-TLS variables. Nil means os.Getenv.
	Getenv func(string) string
}

// Dispatch is the one branch a generated main needs. argv is os.Args[1:]; it
// reports whether the first word was `health` and, if so, what probing did.
// Any other word is left to the next dispatcher (migratecli) or the server.
func Dispatch(ctx context.Context, argv []string, opts Options) (bool, error) {
	if len(argv) == 0 || argv[0] != Command {
		return false, nil
	}
	return true, Run(ctx, argv[1:], opts)
}

const usage = `usage: <binary> health [--timeout DURATION]

Ask every listener this binary opens whether it answers, on this host:
an HTTP listener GET /healthz, a gRPC listener grpc.health.v1 Check.
The addresses come from the same environment the server reads at startup.

Exit 0 when every listener answered, 1 naming the ones that did not.
Liveness only: no database or backend is consulted.

  --timeout DURATION   bound on the whole probe (default 3s)
`

// Run probes every listener concurrently; args are the tokens AFTER `health`.
func Run(ctx context.Context, args []string, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	fs := flag.NewFlagSet(Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", DefaultTimeout, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return nil
		}
		return fmt.Errorf("health: %w\n\n%s", err, usage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("health: unexpected argument %q\n\n%s", fs.Arg(0), usage)
	}
	if *timeout <= 0 {
		return fmt.Errorf("health: --timeout must be positive, got %s", *timeout)
	}
	if len(opts.Probes) == 0 {
		return errors.New("health: this binary opens no listener, so there is nothing to probe")
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	errs := make([]error, len(opts.Probes))
	var wg sync.WaitGroup
	for i, p := range opts.Probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.check(ctx, getenv)
		}()
	}
	wg.Wait()

	var failed []string
	for i, p := range opts.Probes {
		if errs[i] != nil {
			fmt.Fprintf(out, "FAIL %s %s: %v\n", p.Name, p.Addr, errs[i])
			failed = append(failed, p.Name)
			continue
		}
		fmt.Fprintf(out, "ok   %s %s\n", p.Name, p.Addr)
	}
	if len(failed) > 0 {
		return fmt.Errorf("health: %s not answering", strings.Join(failed, ", "))
	}
	return nil
}

func (p Probe) check(ctx context.Context, getenv func(string) string) error {
	tgt, err := target(p.Addr)
	if err != nil {
		return err
	}
	switch p.kind {
	case kindGRPC:
		return checkGRPC(ctx, tgt, getenv)
	case kindPublicGRPC:
		return checkGRPC(ctx, tgt, noEnv)
	default:
		return checkHTTP(ctx, tgt)
	}
}

// target turns a LISTEN address into a DIAL address on this host.
//
// A wildcard bind (":8080", "0.0.0.0:8080", "[::]:8080") is reachable on
// loopback, which is the one address that is this container whatever network
// it sits on. An explicit host is dialled as written: a server bound to one
// interface does not answer on loopback, and probing loopback would report a
// listener that is up as down.
//
// "[::]" dials 127.0.0.1, not ::1. A generated server listens with network
// "tcp", which on "[::]" is dual-stack and accepts IPv4 too; but a container
// on a network without IPv6 has IPv6 disabled and no ::1 on its loopback, so
// dialling ::1 fails ("address not available") against a server that is up —
// a permanently unhealthy container. Measured with disable_ipv6=1: the [::]
// bind succeeds, 127.0.0.1 connects, ::1 does not.
func target(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	if port == "" || port == "0" {
		// `:0` is a test harness asking the kernel for any port; only the
		// running process knows which one it got.
		return "", fmt.Errorf("listen address %q has no fixed port to probe", listen)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func checkHTTP(ctx context.Context, tgt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+tgt+healthz.Path, nil)
	if err != nil {
		return err
	}
	// No redirects: /healthz answers itself, and a listener that redirects
	// it is not the listener the probe meant to ask.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %s", healthz.Path, resp.Status)
	}
	return nil
}

func checkGRPC(ctx context.Context, tgt string, getenv func(string) string) error {
	opts, err := dialOptions(getenv)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(tgt, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if s := resp.GetStatus(); s != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("grpc.health.v1 status %s", s)
	}
	return nil
}

// noEnv reads every variable as unset — the internal-TLS switch off.
func noEnv(string) string { return "" }

// dialOptions dials with the internal-mesh TLS posture the SERVER listens
// with — both read the one stack-wide W17_INTERNAL_TLS switch, so a plaintext
// probe of a TLS listener (or the reverse) cannot happen.
//
// Under TLS the probe dials loopback, which is not a name the leaf was issued
// for. Rather than skip verification, it names the server by the first DNS
// SAN of THIS container's own leaf (W17_INTERNAL_TLS_CERT) — the certificate
// the listener presents — so the chain is still verified against the CA the
// stack trusts.
func dialOptions(getenv func(string) string) ([]grpc.DialOption, error) {
	tlsOpt, err := grpcclient.TLSDialOption(getenv)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{tlsOpt}
	if !grpcclient.InternalTLSEnabled(getenv(grpcclient.EnvInternalTLS)) {
		return opts, nil
	}
	name, err := leafServerName(getenv(grpcclient.EnvInternalTLSCert), getenv(grpcclient.EnvInternalTLSKey))
	if err != nil {
		return nil, err
	}
	return append(opts, grpc.WithAuthority(name)), nil
}

// leafServerName is the name a client verifies this container's own server
// leaf against: its first DNS SAN, else its first IP SAN.
func leafServerName(certPath, keyPath string) (string, error) {
	if certPath == "" || keyPath == "" {
		return "", fmt.Errorf("%s is on but %s / %s are not both set — the listener cannot be serving TLS either",
			grpcclient.EnvInternalTLS, grpcclient.EnvInternalTLSCert, grpcclient.EnvInternalTLSKey)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return "", fmt.Errorf("loading this container's leaf (%s): %w", grpcclient.EnvInternalTLSCert, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("parsing this container's leaf: %w", err)
	}
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0], nil
	}
	if len(leaf.IPAddresses) > 0 {
		return leaf.IPAddresses[0].String(), nil
	}
	return "", errors.New("this container's leaf carries no DNS or IP SAN to verify the listener against")
}
