// Package s3 is the kvfs driver for S3 and S3-compatible object stores
// (MinIO, Cloudflare R2, GCS through its XML API): an upload connection whose
// bytes live in a bucket instead of on one machine's disk, so every replica of
// the gateway and the storage tier reads the same objects.
//
// It speaks the same DSN as the migrator's S3 applier, so one connection has
// one spelling everywhere:
//
//	s3://[<access-key>:<secret>@]<bucket>[/<prefix>][?endpoint=URL][&region=R][&profile=P][&path_style=true|false]
//
// One DSN is one store, credentials included, the way a Postgres DSN carries
// its password: a binary with two stores (a MinIO and an R2 bucket) needs two
// key pairs, which process-wide variables cannot hold. Without them the AWS
// default chain applies — a shared profile, an instance or task role, which is
// where a cloud deployment should leave them. The DSN is therefore a secret,
// and nothing here ever prints it. `endpoint` points at a non-AWS store; path-style addressing is
// the default whenever it is set (MinIO and most compatible stores expect it)
// and virtual-hosted otherwise, and `path_style` overrides either.
//
// Atomicity is the store's own: a PutObject is visible whole or not at all, so
// PutFromTempFile is one PutObject and the staging file is removed only after
// it succeeded.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// Config is a parsed DSN.
type Config struct {
	// AccessKeyID / SecretAccessKey are the DSN's userinfo; empty = the
	// default chain.
	AccessKeyID, SecretAccessKey string
	Bucket                       string
	Prefix                       string // key prefix inside the bucket, without leading or trailing "/"
	Endpoint                     string
	Region                       string
	Profile                      string
	PathStyle                    bool
}

// ParseDSN reads `s3://<bucket>[/<prefix>]?…`. It refuses anything else rather
// than guessing, and never echoes the DSN's query back (it may be pasted from
// somewhere that put a secret in it).
func ParseDSN(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return Config{}, errors.New("kvfs s3: the DSN is not a URL (expected s3://<bucket>[/<prefix>]?endpoint=…)")
	}
	if u.Scheme != "s3" {
		return Config{}, fmt.Errorf("kvfs s3: expected an s3:// DSN, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return Config{}, errors.New("kvfs s3: the DSN names no bucket (expected s3://<bucket>)")
	}
	q := u.Query()
	cfg := Config{}
	if u.User != nil {
		cfg.AccessKeyID = u.User.Username()
		cfg.SecretAccessKey, _ = u.User.Password()
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			return Config{}, errors.New("kvfs s3: the DSN carries half a key pair — give both the access key id and its key in the userinfo, or neither for the default chain")
		}
	}
	cfg = Config{
		AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
		Bucket:   u.Host,
		Prefix:   strings.Trim(u.Path, "/"),
		Endpoint: q.Get("endpoint"),
		Region:   q.Get("region"),
		Profile:  q.Get("profile"),
	}
	cfg.PathStyle = cfg.Endpoint != ""
	if v := q.Get("path_style"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("kvfs s3: path_style=%q is not true or false", v)
		}
		cfg.PathStyle = b
	}
	return cfg, nil
}

// Driver is a kvfs.Driver over one bucket (and prefix).
type Driver struct {
	cli    *awss3.Client
	bucket string
	prefix string
}

var _ kvfs.Driver = (*Driver)(nil)

// New parses dsn and builds the client: the DSN's key pair when it carries
// one, the AWS default chain otherwise. Nothing is dialled here; the first
// call reaches the store.
func New(ctx context.Context, dsn string) (*Driver, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	var load []func(*awsconfig.LoadOptions) error
	if cfg.AccessKeyID != "" {
		load = append(load, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	if cfg.Region != "" {
		load = append(load, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.Profile != "" {
		load = append(load, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, load...)
	if err != nil {
		return nil, fmt.Errorf("kvfs s3: aws config: %w", err)
	}
	if awsCfg.Region == "" {
		// The SDK refuses to sign without one; an S3-compatible store
		// ignores it, so only a missing one is filled.
		awsCfg.Region = "us-east-1"
	}
	cli := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	return &Driver{cli: cli, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// EnsureBucket makes the driver's bucket exist: a HEAD, and a CreateBucket
// only when the store answers that there is none. It is the object-store
// counterpart of local_fs.EnsureRoot, called by the gateway — a connection's
// only writer — at boot, so a fresh dev stack (an empty MinIO) works without
// a provisioning step. Where infrastructure owns the bucket (terraform in the
// cloud targets) the HEAD finds it and nothing is created; a store that
// refuses the HEAD for any other reason (no permission, wrong endpoint) is an
// error, not a reason to try creating it.
func (d *Driver) EnsureBucket(ctx context.Context) error {
	_, err := d.cli.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(d.bucket)})
	if err == nil {
		return nil
	}
	if !isNotFound(err) && !isNoSuchBucket(err) {
		return fmt.Errorf("kvfs s3: bucket %s: %w", d.bucket, err)
	}
	if _, err := d.cli.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(d.bucket)}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) {
			return nil // another replica created it between the HEAD and here
		}
		return fmt.Errorf("kvfs s3: create bucket %s: %w", d.bucket, err)
	}
	return nil
}

func isNoSuchBucket(err error) bool {
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchBucket"
}

func (d *Driver) objectKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if d.prefix == "" {
		return key
	}
	return d.prefix + "/" + key
}

// Open streams the object.
func (d *Driver) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := d.cli.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.objectKey(key))})
	if err != nil {
		return nil, d.wrap("open", key, err)
	}
	return out.Body, nil
}

// Stat reports size and last modification.
func (d *Driver) Stat(ctx context.Context, key string) (kvfs.Info, error) {
	out, err := d.cli.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.objectKey(key))})
	if err != nil {
		return kvfs.Info{}, d.wrap("stat", key, err)
	}
	info := kvfs.Info{Size: aws.ToInt64(out.ContentLength)}
	if out.LastModified != nil {
		info.ModTime = *out.LastModified
	}
	return info, nil
}

// OpenSeekable returns a reader that fetches ranges on demand: a Range request
// served through http.ServeContent downloads the requested bytes, not the
// whole object.
func (d *Driver) OpenSeekable(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	info, err := d.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &rangeReader{ctx: ctx, d: d, key: key, size: info.Size}, nil
}

// PutFromTempFile uploads tmpPath as key in one PutObject and removes the file
// once the store has it. On failure the file is left for the caller.
func (d *Driver) PutFromTempFile(ctx context.Context, key, tmpPath string) (string, error) {
	f, err := os.Open(tmpPath) // #nosec G304 -- the caller's own staging file
	if err != nil {
		return "", fmt.Errorf("kvfs s3: put %s: %w", key, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("kvfs s3: put %s: %w", key, err)
	}
	_, err = d.cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(d.bucket), Key: aws.String(d.objectKey(key)),
		Body: f, ContentLength: aws.Int64(st.Size()),
	})
	_ = f.Close()
	if err != nil {
		return "", d.wrap("put", key, err)
	}
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("kvfs s3: put %s: stored, but the staging file stays: %w", key, err)
	}
	return key, nil
}

// Delete removes the object; a missing one is not an error (S3's own
// DeleteObject answers success for it).
func (d *Driver) Delete(ctx context.Context, key string) error {
	if _, err := d.cli.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.objectKey(key))}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return d.wrap("delete", key, err)
	}
	return nil
}

func (d *Driver) wrap(op, key string, err error) error {
	if isNotFound(err) {
		return kvfs.ErrNotFound
	}
	return fmt.Errorf("kvfs s3: %s %s in bucket %s: %w", op, key, d.bucket, err)
}

// isNotFound recognises a missing object across the shapes S3 and compatible
// stores answer with: NoSuchKey on GET, a bare 404 (no body) on HEAD.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
		return true
	}
	var resp *awshttp.ResponseError
	return errors.As(err, &resp) && resp.HTTPStatusCode() == http.StatusNotFound
}

// rangeReader reads one object through ranged GETs, opened lazily from the
// current offset and dropped on every seek that moves it.
type rangeReader struct {
	ctx  context.Context
	d    *Driver
	key  string
	size int64
	off  int64
	body io.ReadCloser
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		out, err := r.d.cli.GetObject(r.ctx, &awss3.GetObjectInput{
			Bucket: aws.String(r.d.bucket), Key: aws.String(r.d.objectKey(r.key)),
			Range: aws.String("bytes=" + strconv.FormatInt(r.off, 10) + "-"),
		})
		if err != nil {
			return 0, r.d.wrap("read", r.key, err)
		}
		r.body = out.Body
	}
	n, err := r.body.Read(p)
	r.off += int64(n)
	if errors.Is(err, io.EOF) && r.off < r.size {
		// The object shrank under us, or the store cut the body short.
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *rangeReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.off + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return r.off, fmt.Errorf("kvfs s3: seek: whence %d", whence)
	}
	if next < 0 {
		return r.off, errors.New("kvfs s3: seek before the start")
	}
	if next != r.off && r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	r.off = next
	return next, nil
}

func (r *rangeReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
