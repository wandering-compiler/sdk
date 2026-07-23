package s3_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := s3.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestNew_BadSchemeRefuses(t *testing.T) {
	_, err := s3.New(context.Background(), "postgres://x")
	if err == nil || !strings.Contains(err.Error(), "expected s3:// scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestParseDSN_BucketOnly(t *testing.T) {
	got, err := s3.ParseDSN("s3://my-bucket")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if got.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q", got.Bucket)
	}
	if got.Endpoint != "" || got.Region != "" || got.Profile != "" {
		t.Errorf("expected empty optional params, got %+v", got)
	}
}

func TestParseDSN_FullConfig(t *testing.T) {
	got, err := s3.ParseDSN("s3://bkt?endpoint=http://minio:9000&region=us-east-1&profile=staging")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if got.Bucket != "bkt" {
		t.Errorf("Bucket = %q", got.Bucket)
	}
	if got.Endpoint != "http://minio:9000" {
		t.Errorf("Endpoint = %q", got.Endpoint)
	}
	if got.Region != "us-east-1" {
		t.Errorf("Region = %q", got.Region)
	}
	if got.Profile != "staging" {
		t.Errorf("Profile = %q", got.Profile)
	}
}

func TestParseDSN_BadScheme(t *testing.T) {
	_, err := s3.ParseDSN("redis://x")
	if err == nil || !strings.Contains(err.Error(), "expected s3://") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestParseDSN_NoBucket(t *testing.T) {
	_, err := s3.ParseDSN("s3://")
	if err == nil || !strings.Contains(err.Error(), "missing bucket") {
		t.Errorf("expected missing-bucket error, got %v", err)
	}
}

func TestParseDSN_MalformedURL(t *testing.T) {
	_, err := s3.ParseDSN("://broken")
	if err == nil || !strings.Contains(err.Error(), "parse url") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestClose_NoOp(t *testing.T) {
	a, err := s3.New(context.Background(), "s3://my-bucket")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
