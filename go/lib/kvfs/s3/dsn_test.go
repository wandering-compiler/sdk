package s3

import (
	"strings"
	"testing"
)

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		want Config
	}{
		{"s3://media", Config{Bucket: "media"}},
		{"s3://media/uploads/", Config{Bucket: "media", Prefix: "uploads"}},
		// An endpoint is a non-AWS store, which expects path-style addressing.
		{"s3://media?endpoint=http://minio:9000&region=eu-1", Config{Bucket: "media", Endpoint: "http://minio:9000", Region: "eu-1", PathStyle: true}},
		{"s3://media?endpoint=https://r2.example&path_style=false", Config{Bucket: "media", Endpoint: "https://r2.example"}},
		{"s3://media?path_style=true&profile=ops", Config{Bucket: "media", Profile: "ops", PathStyle: true}},
	} {
		got, err := ParseDSN(tc.dsn)
		if err != nil {
			t.Errorf("%s: %v", tc.dsn, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.dsn, got, tc.want)
		}
	}
}

func TestParseDSN_Refuses(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"https://media", "expected an s3:// DSN"},
		{"s3://", "names no bucket"},
		{"s3://key:secret@media", "credentials do not belong in the DSN"},
		{"s3://media?path_style=maybe", "path_style"},
	} {
		_, err := ParseDSN(tc.dsn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.dsn, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the refusal echoes the DSN's secret: %v", tc.dsn, err)
		}
	}
}
