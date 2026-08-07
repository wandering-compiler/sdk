// Package s3 is the production S3 Applier. Talks to S3 via the
// AWS SDK for Go v2 (`github.com/aws/aws-sdk-go-v2/service/s3`);
// no shell-out, no `aws` CLI dep on the deploy host. minio /
// other S3-compatible endpoints are reachable via the
// `endpoint=...` DSN parameter.
//
// **Renderer command shape.** The applied-state Renderer
// (Phase C.4 + applied.S3()) emits a custom dialect-private
// shape for bookkeeping:
//
//   - `S3 PUT wc-migrations/<ts>.json {json}`  → PutObject
//   - `S3 DELETE wc-migrations/<ts>.json`     → DeleteObject
//   - `S3 DELETE_PREFIX wc-migrations/`        → recursive delete
//
// User DDL (AddTable / DropTable / RenameTable) the migrator
// emits as `aws s3 rm --recursive` / `aws s3 mv --recursive`
// CLI markers; this Applier doesn't currently dispatch those —
// the user_progression scenario shape is pure no-op markers
// (S3 schemaless), so all real work is the Renderer's
// bookkeeping ops. CLI-shaped DDL would land via a parallel
// parser when a non-bookkeeping op path is exercised.
//
// DSN form: `s3://<bucket>[?endpoint=URL][&region=...][&profile=...]`.
// Bucket is connection config — the Renderer's command shapes
// reference keys without bucket; the Applier prefixes the
// configured bucket on every PutObject / DeleteObject.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// trackerPrefix is the well-known object-key prefix used for
// applied-state bookkeeping (Phase C.4 + applied.S3()).
// AppliedHead lists this prefix; the Renderer's RecordVersion
// / RemoveVersion / DropTracker reference it by this exact
// string.
const trackerPrefix = "wc-migrations/"

// Applier owns one lazy *s3.Client. PutObject / DeleteObject /
// ListObjectsV2 are concurrency-safe on a single client; calls
// from multiple goroutines share one instance.
type Applier struct {
	bucket   string
	endpoint string
	region   string
	profile  string

	mu     sync.Mutex
	client *awss3.Client

	// parallelOverride is the operator-supplied
	// `--parallel <N>` from the CLI. 0 = no override; YAML
	// data migrations fall back to the migration's project-
	// side `parallel:` field, then to datamigrate.DefaultParallel.
	// Only meaningful for YAML data migrations (Phase E v2.1);
	// command-style bodies ignore it.
	parallelOverride int

	// Run-lock tunables (Q48-datamigrate-1). nowFn defaults to
	// time.Now; lockStale / lockBeat default to the runlock package
	// values. Tests inject a fake clock + short windows to exercise
	// the stale-takeover / heartbeat paths deterministically.
	nowFn     func() time.Time
	lockStale time.Duration
	lockBeat  time.Duration
}

var _ migrate.Applier = (*Applier)(nil)

// New parses the DSN + builds a lazy-connect Applier. AWS
// credentials / endpoint resolution happen on first
// Apply / AppliedHead.
func New(_ context.Context, dsn string) (*Applier, error) {
	if dsn == "" {
		return nil, fmt.Errorf("s3.New: dsn is empty")
	}
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("s3.New: %w", err)
	}
	return &Applier{
		bucket:   cfg.Bucket,
		endpoint: cfg.Endpoint,
		region:   cfg.Region,
		profile:  cfg.Profile,
	}, nil
}

// AppliedHead lists keys under `wc-migrations/` and returns
// the max-by-lex `<ts>` (with `.json` suffix stripped). Empty
// prefix → "" (no migrations applied yet). NoSuchBucket is
// surfaced as an error; the deploy client is responsible for
// bucket provisioning.
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	cli, err := a.s3Client(ctx)
	if err != nil {
		return "", err
	}
	var head string
	var token *string
	for {
		out, err := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(a.bucket),
			Prefix:            aws.String(trackerPrefix),
			ContinuationToken: token,
		})
		if err != nil {
			return "", fmt.Errorf("s3 ListObjectsV2 %s/%s: %w", a.bucket, trackerPrefix, err)
		}
		for _, obj := range out.Contents {
			k := aws.ToString(obj.Key)
			ts := strings.TrimSuffix(strings.TrimPrefix(k, trackerPrefix), ".json")
			if ts > head {
				head = ts
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return head, nil
}

// Apply runs the migration body lines through the typed S3 API.
//
// Phase E (D-iter3-15): bodies whose up_sql is a YAML data
// migration dispatch through applyYAMLDataMigration instead —
// per-object ListObjectsV2+GetObject+modify+PutObject loop
// driven by parsed Operations[]. Detection via
// lib/datamigrate.LooksLikeYAML.
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if datamigrate.LooksLikeYAML([]byte(m.GetUpSql())) {
		return a.applyYAMLDataMigration(ctx, m)
	}
	if err := a.run(ctx, m.GetUpSql()); err != nil {
		return fmt.Errorf("s3 apply up_sql: %w", err)
	}
	if pt := m.GetUpPostTx(); pt != "" {
		if err := a.run(ctx, pt); err != nil {
			return fmt.Errorf("s3 apply up_post_tx: %w", err)
		}
	}
	return nil
}

// Rollback runs the migration's down payload. Order: down_pre_tx
// first, then down_sql. applied.S3() injects the
// `S3 DELETE wc-migrations/<ts>.json` bookkeeping erase into
// the down body; user-side down ops (DELETE_PREFIX etc.) live
// alongside.
//
// Phase E: YAML data migration bodies dispatch through
// rollbackYAMLDataMigration. The down body is either an
// auto-derived YAML inverse or a `# wc:irreversible:` comment
// block — the latter refuses without --allow-irreversible.
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	if datamigrate.LooksLikeYAML([]byte(m.GetUpSql())) {
		return a.rollbackYAMLDataMigration(ctx, m)
	}
	if err := a.run(ctx, m.GetDownPreTx()); err != nil {
		return fmt.Errorf("s3 rollback down_pre_tx: %w", err)
	}
	if err := a.run(ctx, m.GetDownSql()); err != nil {
		return fmt.Errorf("s3 rollback down_sql: %w", err)
	}
	return nil
}

// Close releases the underlying SDK client (no persistent
// connection to tear down beyond the HTTP transport, which
// the SDK manages internally).
func (a *Applier) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.client = nil
	return nil
}

// SetParallelOverride installs the operator's `--parallel <N>`
// CLI value. Forwarded by the factory at construction. 0 = no
// override (fall through to the YAML migration's `parallel:`
// field, then datamigrate.DefaultParallel).
//
// Only consulted by YAML data migrations (Phase E v2.1); the
// command-style migration path runs sequentially regardless.
func (a *Applier) SetParallelOverride(n int) {
	a.parallelOverride = n
}

// run iterates each line of script. Empty / `#` comment
// lines are skipped; `S3 <verb>` lines dispatch via runOne.
// Lines that don't start with `S3 ` are silently ignored
// (today: `aws s3 ...` markers from user-DDL emit, which
// the user_progression scenario doesn't exercise).
func (a *Applier) run(ctx context.Context, script string) error {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "S3 ") {
			// User-DDL `aws s3 ...` markers fall here; the
			// scenarios exercised today are all no-op markers
			// (S3 schemaless), so silently skipping is safe.
			// When a non-bookkeeping op path needs real S3 work,
			// add an `aws s3` parser here.
			continue
		}
		if err := a.runOne(ctx, line); err != nil {
			return err
		}
	}
	return nil
}

// runOne dispatches one `S3 <verb> ...` line to the typed S3
// API. Verbs:
//
//   - `S3 PUT <key> <body...>`     → PutObject (body is the
//     rest of the line verbatim, including spaces — the
//     Renderer emits JSON which has no embedded newlines)
//   - `S3 DELETE <key>`            → DeleteObject
//   - `S3 DELETE_PREFIX <prefix>`  → ListObjectsV2 + DeleteObjects
func (a *Applier) runOne(ctx context.Context, line string) error {
	rest := strings.TrimPrefix(line, "S3 ")

	cli, err := a.s3Client(ctx)
	if err != nil {
		return err
	}

	switch {
	case strings.HasPrefix(rest, "PUT "):
		body := strings.TrimPrefix(rest, "PUT ")
		// `PUT <key> <body>` — split on the FIRST space only;
		// body keeps its embedded spaces. Empty body is OK
		// (corresponds to a zero-byte object).
		i := strings.IndexByte(body, ' ')
		if i < 0 {
			return fmt.Errorf("s3: PUT missing body in %q", line)
		}
		key, payload := body[:i], body[i+1:]
		_, err := cli.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(a.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte(payload)),
		})
		if err != nil {
			return fmt.Errorf("s3 PutObject %s/%s: %w", a.bucket, key, err)
		}
		return nil

	case strings.HasPrefix(rest, "DELETE_PREFIX "):
		prefix := strings.TrimSpace(strings.TrimPrefix(rest, "DELETE_PREFIX "))
		if prefix == "" {
			return fmt.Errorf("s3: DELETE_PREFIX missing prefix in %q", line)
		}
		return a.deletePrefix(ctx, cli, prefix)

	case strings.HasPrefix(rest, "DELETE "):
		key := strings.TrimSpace(strings.TrimPrefix(rest, "DELETE "))
		if key == "" {
			return fmt.Errorf("s3: DELETE missing key in %q", line)
		}
		_, err := cli.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(a.bucket),
			Key:    aws.String(key),
		})
		if err != nil && !isNoSuchKey(err) {
			return fmt.Errorf("s3 DeleteObject %s/%s: %w", a.bucket, key, err)
		}
		return nil

	default:
		return fmt.Errorf("s3: unsupported S3 verb in %q", line)
	}
}

// deletePrefix lists every object under `prefix` and deletes
// them in batches of 1000 (the DeleteObjects max). NoSuchBucket
// is surfaced; empty prefix is a soft success.
func (a *Applier) deletePrefix(ctx context.Context, cli *awss3.Client, prefix string) error {
	var token *string
	for {
		out, err := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(a.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("s3 ListObjectsV2 %s/%s: %w", a.bucket, prefix, err)
		}
		if len(out.Contents) > 0 {
			ids := make([]types.ObjectIdentifier, 0, len(out.Contents))
			for _, obj := range out.Contents {
				ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
			}
			dout, err := cli.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
				Bucket: aws.String(a.bucket),
				Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
			})
			if err != nil {
				return fmt.Errorf("s3 DeleteObjects %s/%s: %w", a.bucket, prefix, err)
			}
			// S3 returns HTTP 200 with per-object failures in
			// dout.Errors even when the top-level err is nil. Surface
			// them — a DELETE_PREFIX in a down/rollback body must not
			// report success while leaving objects behind.
			if len(dout.Errors) > 0 {
				var b strings.Builder
				for i, e := range dout.Errors {
					if i > 0 {
						b.WriteString("; ")
					}
					fmt.Fprintf(&b, "%s: %s (%s)", aws.ToString(e.Key), aws.ToString(e.Message), aws.ToString(e.Code))
				}
				return fmt.Errorf("s3 DeleteObjects %s/%s: %d object(s) failed: %s",
					a.bucket, prefix, len(dout.Errors), b.String())
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// s3Client builds the *s3.Client lazily on first use.
// Endpoint / region / profile come from DSN config; AWS
// credentials use the SDK default chain (env, shared
// credentials, IAM role, etc.) plus optional `--profile`.
func (a *Applier) s3Client(ctx context.Context) (*awss3.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if a.region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(a.region))
	}
	if a.profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(a.profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	clientOpts := []func(*awss3.Options){
		// Path-style addressing is what minio + most S3-compat
		// stores expect. AWS itself accepts both.
		func(o *awss3.Options) { o.UsePathStyle = true },
	}
	if a.endpoint != "" {
		ep := a.endpoint
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.BaseEndpoint = aws.String(ep)
		})
	}
	a.client = awss3.NewFromConfig(cfg, clientOpts...)
	return a.client, nil
}

// isNoSuchKey returns true when the underlying API error is
// the "object not found" shape — used to make DeleteObject
// idempotent for rollback paths.
func isNoSuchKey(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

// DSNConfig is the parsed shape exposed for tests.
type DSNConfig struct {
	Bucket   string
	Endpoint string
	Region   string
	Profile  string
}

// ParseDSN — `s3://<bucket>[?endpoint=URL][&region=...][&profile=...]`.
func ParseDSN(dsn string) (DSNConfig, error) {
	if dsn == "" {
		return DSNConfig{}, fmt.Errorf("s3: dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return DSNConfig{}, fmt.Errorf("s3: parse url: %w", err)
	}
	if u.Scheme != "s3" {
		return DSNConfig{}, fmt.Errorf("s3: expected s3:// scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return DSNConfig{}, fmt.Errorf("s3: missing bucket (expected s3://<bucket>)")
	}
	q := u.Query()
	return DSNConfig{
		Bucket:   u.Host,
		Endpoint: q.Get("endpoint"),
		Region:   q.Get("region"),
		Profile:  q.Get("profile"),
	}, nil
}

// Validate enforces invariants on a parsed DSNConfig.
func Validate(cfg DSNConfig) error {
	if cfg.Bucket == "" {
		return fmt.Errorf("s3: empty bucket")
	}
	return nil
}
