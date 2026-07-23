package s3

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Snapshotter is the S3 dump/restore driver for the dev DB lifecycle.
// It mirrors every object in the configured bucket: Dump lists the
// whole bucket and streams a gob record (key + body) per object;
// Restore PutObjects each back. It reuses the Applier's lazy client +
// DSN parsing so the snapshot path shares the apply path's connection
// semantics (endpoint / region / profile / path-style addressing).
//
// Restore is additive-overwrite: it PUTs every snapshot object (so a
// same-key object is overwritten) but does NOT delete objects present
// in the target yet absent from the snapshot. In the reconcile flow
// restore runs into a freshly wiped/built bucket, so strays don't
// arise in practice; a developer restoring over a dirty bucket should
// empty it first. Documented in the spec's S2 lossy notes.
type Snapshotter struct {
	app *Applier
}

// snapObject is one mirrored object.
type snapObject struct {
	Key  string
	Body []byte
}

// NewSnapshotter reuses the Applier constructor (DSN parse + lazy
// client), so a bad DSN is rejected here exactly as for apply.
func NewSnapshotter(dsn string) (*Snapshotter, error) {
	app, err := New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	return &Snapshotter{app: app}, nil
}

// Dump lists the whole bucket and writes a gob record per object.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	cli, err := s.app.s3Client(ctx)
	if err != nil {
		return err
	}
	enc := gob.NewEncoder(w)
	var token *string
	for {
		out, err := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.app.bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("s3 Dump: ListObjectsV2 %s: %w", s.app.bucket, err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			body, err := s.getObject(ctx, cli, key)
			if err != nil {
				return err
			}
			if err := enc.Encode(snapObject{Key: key, Body: body}); err != nil {
				return fmt.Errorf("s3 Dump: encode %q: %w", key, err)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
}

// getObject reads one object's full body into memory. Dev objects are
// small (KB-scale config/seed blobs); streaming straight to the gob
// record would interleave with the encoder, so we buffer per object.
func (s *Snapshotter) getObject(ctx context.Context, cli *awss3.Client, key string) ([]byte, error) {
	out, err := cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.app.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 Dump: GetObject %q: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, out.Body); err != nil {
		return nil, fmt.Errorf("s3 Dump: read %q: %w", key, err)
	}
	return buf.Bytes(), nil
}

// Restore PutObjects each gob record back into the bucket.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	cli, err := s.app.s3Client(ctx)
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(r)
	for {
		var obj snapObject
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("s3 Restore: decode: %w", err)
		}
		if _, err := cli.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(s.app.bucket),
			Key:    aws.String(obj.Key),
			Body:   bytes.NewReader(obj.Body),
		}); err != nil {
			return fmt.Errorf("s3 Restore: PutObject %q: %w", obj.Key, err)
		}
	}
	return nil
}
