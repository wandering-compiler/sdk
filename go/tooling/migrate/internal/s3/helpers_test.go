package s3

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
)

// TestRunOne_EmptyKeyGuards pins the defensive empty-key guards in
// runOne for DELETE / DELETE_PREFIX. These are unreachable through
// Apply (run() trims trailing whitespace off every line) so they
// are exercised directly. s3Client builds lazily from static env
// creds and makes no network call before the guard fires.
func TestRunOne_EmptyKeyGuards(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	a, err := New(context.Background(), "s3://bucket")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	cases := []struct {
		line string
		frag string
	}{
		{"S3 DELETE ", "DELETE missing key"},
		{"S3 DELETE_PREFIX ", "DELETE_PREFIX missing prefix"},
	}
	for _, tc := range cases {
		if err := a.runOne(context.Background(), tc.line); err == nil ||
			!strings.Contains(err.Error(), tc.frag) {
			t.Errorf("runOne(%q) = %v, want fragment %q", tc.line, err, tc.frag)
		}
	}
}

// TestIsNoSuchKey pins the "object not found" predicate used to
// make DeleteObject / GetObject idempotent. NoSuchKey + NotFound
// match; everything else (other API codes, plain errors, nil)
// does not.
func TestIsNoSuchKey(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey", &smithy.GenericAPIError{Code: "NoSuchKey"}, true},
		{"NotFound", &smithy.GenericAPIError{Code: "NotFound"}, true},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoSuchKey(tc.err); got != tc.want {
				t.Errorf("isNoSuchKey(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsNoSuchKey_WrappedAPIError pins that errors.As unwrapping
// finds an APIError nested behind fmt.Errorf %w wrapping.
func TestIsNoSuchKey_WrappedAPIError(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), &smithy.GenericAPIError{Code: "NoSuchKey"})
	if !isNoSuchKey(wrapped) {
		t.Error("wrapped NoSuchKey should still match")
	}
}

// TestBuildCodec_JSONEncodingReturnsNil pins the JSON short-
// circuit: no protobuf codec is built.
func TestBuildCodec_JSONEncodingReturnsNil(t *testing.T) {
	codec, err := buildCodec(&datamigrate.Migration{Encoding: datamigrate.EncodingJSON})
	if err != nil || codec != nil {
		t.Errorf("buildCodec(json) = (%v,%v), want (nil,nil)", codec, err)
	}
}

// TestBuildCodec_ProtobufErrors pins the two refusal branches:
// un-decodable base64, then valid base64 that is not an FDS.
func TestBuildCodec_ProtobufErrors(t *testing.T) {
	if _, err := buildCodec(&datamigrate.Migration{
		Encoding:        datamigrate.EncodingProtobuf,
		ProtoDescriptor: "!!!not-base64!!!",
		ProtoMessage:    "pkg.User",
	}); err == nil {
		t.Error("expected base64 decode error")
	}
	if _, err := buildCodec(&datamigrate.Migration{
		Encoding:        datamigrate.EncodingProtobuf,
		ProtoDescriptor: "aGVsbG8=", // "hello", not an FDS
		ProtoMessage:    "pkg.User",
	}); err == nil {
		t.Error("expected proto codec error for non-FDS bytes")
	}
}

// TestBuildTransformVMs pins the up-front compile: nil when no
// transform ops, a keyed map for a good script, and an op-indexed
// error for a bad one.
func TestBuildTransformVMs(t *testing.T) {
	none, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{{Op: datamigrate.OpAddFieldDefault}},
	})
	if err != nil || none != nil {
		t.Errorf("no transform ops should yield (nil,nil), got (%v,%v)", none, err)
	}

	good, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{
			{Op: datamigrate.OpAddFieldDefault},
			{Op: datamigrate.OpTransformField, ScriptLang: "starlark", Script: "def transform(value):\n    return value\n"},
		},
	})
	if err != nil {
		t.Fatalf("buildTransformVMs(good): %v", err)
	}
	if good[1] == nil {
		t.Error("expected VM at op index 1")
	}

	if _, err := buildTransformVMs(&datamigrate.Migration{
		Operations: []datamigrate.Operation{
			{Op: datamigrate.OpTransformField, ScriptLang: "starlark", Script: "x = 1\n"},
		},
	}); err == nil || !strings.Contains(err.Error(), "op[0]") {
		t.Errorf("expected op[0] compile error, got %v", err)
	}
}

// TestIsIrreversibleMarkerBody pins the comment-block detector
// distinguishing a refused rollback from an applicable YAML body.
func TestIsIrreversibleMarkerBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"", false},
		{"   \n\t\n", false},
		{"# wc:irreversible: REMOVE_FIELD has no inverse", true},
		{"\n# header\n\n# wc:irreversible: x\n", true},
		{"# just a note\n# another", false},
		{"version: 1\noperations: []", false},
	}
	for _, tc := range cases {
		if got := isIrreversibleMarkerBody(tc.body); got != tc.want {
			t.Errorf("isIrreversibleMarkerBody(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestValidate_S3Config pins the empty-bucket guard.
func TestValidate_S3Config(t *testing.T) {
	if err := Validate(DSNConfig{}); err == nil {
		t.Error("empty bucket should be refused")
	}
	if err := Validate(DSNConfig{Bucket: "b"}); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
}
