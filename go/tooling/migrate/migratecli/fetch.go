package migratecli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Fetching from a deployed binary, and the one thing it costs.
//
// `apply --fetch` is the shape a read-only container wants: pull the set from
// the console, apply it, never touch a disk. Nothing in it is new machinery —
// it is the same MigrationFetch RPC `w17ctl migrate fetch` calls, from a
// process that happens to also hold the database.
//
// ⚠️ It puts a CONSOLE CREDENTIAL in the deployment. A box that holds only a
// database password today would hold a console token too. That is a real
// widening and it is why `apply --migrations <dir>` stays: an operator who
// would rather fetch on a workstation and ship artefacts can still do exactly
// that, and never set W17_CONSOLE_TOKEN at all.
//
// The token is an API token (the auth plugin issues them: CreateApiToken /
// ListApiTokens / RevokeApiToken), not a login session — a deployment should
// carry a credential someone can revoke on its own without logging anybody
// out.

const (
	// envConsoleAddr is the console this binary fetches from.
	envConsoleAddr = "W17_CONSOLE_ADDR"
	// envConsoleToken is the bearer presented to it.
	envConsoleToken = "W17_CONSOLE_TOKEN" // #nosec G101 -- the NAME of the env var a token is read FROM, not a token
	// envConsoleOrg is the active organisation, when the account is in more
	// than one. A single-org account can leave it unset — the console infers.
	envConsoleOrg = "W17_CONSOLE_ORG"
	// envSkipVerify is the DEV escape hatch for a console serving a
	// throwaway self-signed cert. Same variable w17ctl has always honoured,
	// deliberately: a second spelling would mean an operator who set the one
	// they knew about got a TLS failure from the half that had not heard of
	// it.
	envSkipVerify = "W17_CONSOLE_TLS_SKIP_VERIFY"
	// envConsoleCA points at a PEM bundle to trust IN ADDITION to the system
	// roots. An on-prem console behind a private CA is the case this exists
	// for; without it the only way to reach one is the skip-verify hatch,
	// which turns "trust this specific issuer" into "trust anyone".
	envConsoleCA = "W17_CONSOLE_CA"
)

// fetchMigrations pulls every migration up to each target's pin.
func fetchMigrations(ctx context.Context, projectID string, targets []migrate.ConnTarget, getenv func(string) string) ([]*applyfetchpb.Migration, error) {
	addr := getenv(envConsoleAddr)
	if addr == "" {
		return nil, fmt.Errorf(
			"migrate: --fetch needs a console address\n"+
				"  fix: set %s to the console's gRPC endpoint (e.g. grpcs://api.w17.app:50051),\n"+
				"       or drop --fetch and point --migrations at artefacts fetched elsewhere",
			envConsoleAddr,
		)
	}
	var want []*applyfetchpb.Target
	for _, t := range targets {
		if t.TargetMigrationID == "" {
			continue // never schema-pushed; nothing to fetch
		}
		want = append(want, &applyfetchpb.Target{
			Connection:      t.Connection,
			UpToMigrationId: t.TargetMigrationID,
		})
	}
	if len(want) == 0 {
		return nil, nil
	}

	conn, err := dialFetch(addr, getenv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	resp, err := applyfetchpb.NewMigrationFetchClient(conn).FetchMigrations(ctx, &applyfetchpb.FetchMigrationsRequest{
		ProjectId: projectID,
		Targets:   want,
	})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return nil, fmt.Errorf(
				"migrate: %s refused the fetch: not authenticated\n"+
					"  fix: set %s to an API token (the console issues them: CreateApiToken)\n"+
					"  why: a deploy needs a credential of its own rather than a person's login\n"+
					"       session, so it can be revoked without logging anybody out\n"+
					"  raw: %v",
				addr, envConsoleToken, err,
			)
		}
		return nil, fmt.Errorf("migrate: fetch from %s: %w", addr, err)
	}
	return resp.GetMigrations(), nil
}

// fetchMaxRecvMsgSize bounds one fetch response. A response carries every
// migration body up to the pin, which outgrows gRPC's 4 MiB default on any
// real schema history — the same reason w17ctl lifts it.
const fetchMaxRecvMsgSize = 256 << 20

func dialFetch(addr string, getenv func(string) string) (*grpc.ClientConn, error) {
	target, creds := transportFor(addr, getenv)
	conn, err := grpc.NewClient(target,
		creds,
		grpc.WithPerRPCCredentials(bearerCreds{getenv: getenv}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(fetchMaxRecvMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect %s: %w", addr, err)
	}
	return conn, nil
}

// transportFor strips the scheme and decides TLS.
//
// `grpcs://` and a bare host:port both mean TLS; only an explicit `grpc://`
// opts out, and that is for a sidecar or a loopback proxy where the operator
// has decided the network is the trust boundary. Defaulting the BARE form to
// TLS rather than to plaintext is deliberate: the failure mode of guessing
// wrong in the other direction is a bearer token sent in the clear, and it
// fails silently — the call succeeds.
func transportFor(addr string, getenv func(string) string) (string, grpc.DialOption) {
	if rest, ok := strings.CutPrefix(addr, "grpc://"); ok {
		return rest, grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	return strings.TrimPrefix(addr, "grpcs://"),
		grpc.WithTransportCredentials(credentials.NewTLS(consoleTLSConfig(getenv)))
}

// consoleTLSConfig is the VERIFICATION policy, kept apart from the transport
// choice above so both arms of that choice stay readable side by side.
//
// System roots by default. W17_CONSOLE_CA adds a private CA — additive, since
// a console behind one is usually one of several endpoints a deployment
// talks to. W17_CONSOLE_TLS_SKIP_VERIFY is the last-resort dev hatch, and it
// is the same variable w17ctl reads: one endpoint, one spelling.
func consoleTLSConfig(getenv func(string) string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if path := getenv(envConsoleCA); path != "" {
		if pool, err := caPool(path); err == nil {
			cfg.RootCAs = pool
		}
		// An unreadable CA is deliberately NOT fatal: the dial then fails
		// verification and says so, which is a clearer story than a config
		// error thrown before anything was attempted — and it fails CLOSED,
		// because the system roots do not vouch for a private CA.
	}
	if strings.EqualFold(getenv(envSkipVerify), "true") {
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: explicit DEV escape hatch behind a named env knob; the default verifies against system roots.
	}
	return cfg
}

// caPool builds a root pool of the system roots PLUS the PEM bundle at path.
// Additive rather than replacing: a console reached through a private CA is
// usually one of several endpoints a deployment talks to.
func caPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- an operator-supplied CA bundle path is the input
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", path)
	}
	return pool, nil
}

// bearerCreds attaches the API token, and the active org when one is set.
type bearerCreds struct{ getenv func(string) string }

// GetRequestMetadata attaches the token when there is one, and NOTHING when
// there is not.
//
// An empty token is deliberately not an error here. Whether the fetch surface
// is gated is the CONSOLE's policy, not this client's to assume: a dev or
// in-process console serves it ungated, and refusing to dial one because no
// token was set would be this code deciding a question it does not own. The
// server's own refusal is the answer, and fetchMigrations turns that into the
// sentence naming the variable to set.
func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	token := b.getenv(envConsoleToken)
	if token == "" {
		return nil, nil
	}
	md := map[string]string{"authorization": "Bearer " + token}
	if org := b.getenv(envConsoleOrg); org != "" {
		md["w17-org"] = org
	}
	return md, nil
}

// RequireTransportSecurity reports false so a deliberate `grpc://` sidecar
// dial still carries the bearer. TLS is the default for every other form —
// see transportFor.
func (bearerCreds) RequireTransportSecurity() bool { return false }
