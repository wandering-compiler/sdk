package paging

import (
	"encoding/base64"
	"errors"
	"testing"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/proto"
)

var icKey = []byte("internal-cursor-test-key-0123456789")

// signed builds a `payload "." tag` token over arbitrary PageCursor bytes
// using the real cursorMAC — so MAC verification passes and decoding
// proceeds to the proto-unmarshal stages we want to exercise.
func signed(pcBytes []byte) string {
	payload := base64.RawURLEncoding.EncodeToString(pcBytes)
	tag := base64.RawURLEncoding.EncodeToString(cursorMAC(pcBytes, icKey))
	return payload + "." + tag
}

// 172: a token whose payload segment is not valid base64url → malformed.
func TestDecodeCursor_BadBase64Payload(t *testing.T) {
	got := &w17pb.KeysetValue{}
	_, _, _, _, err := DecodeCursor("@@@bad@@@.sometag", got, 0, icKey)
	if !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("bad base64 payload: want malformed, got %v", err)
	}
}

// 177: a valid base64 payload but a non-base64 tag segment → malformed.
func TestDecodeCursor_BadBase64Tag(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte("anything"))
	got := &w17pb.KeysetValue{}
	_, _, _, _, err := DecodeCursor(payload+".@@@bad@@@", got, 0, icKey)
	if !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("bad base64 tag: want malformed, got %v", err)
	}
}

// 187: payload decodes + MAC verifies, but the bytes do not parse as a
// PageCursor proto → malformed.
func TestDecodeCursor_PayloadNotAPageCursor(t *testing.T) {
	// A truncated varint (field 1, no value) is an invalid proto wire.
	token := signed([]byte{0x08})
	got := &w17pb.KeysetValue{}
	_, _, _, _, err := DecodeCursor(token, got, 0, icKey)
	if !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("non-PageCursor payload: want malformed, got %v", err)
	}
}

// 197: a well-formed, correctly-MAC'd PageCursor whose schema_version
// matches, but whose embedded request bytes do not parse into the caller's
// concrete request type → malformed (before the boundary loop).
func TestDecodeCursor_BadRequestBytes(t *testing.T) {
	const sv = 7
	pc := &w17pb.PageCursor{
		Request:       []byte{0x08}, // truncated varint → won't unmarshal
		SchemaVersion: sv,
	}
	pcBytes, err := proto.Marshal(pc)
	if err != nil {
		t.Fatalf("marshal pc: %v", err)
	}
	got := &w17pb.KeysetValue{}
	_, _, _, _, err = DecodeCursor(signed(pcBytes), got, sv, icKey)
	if !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("bad request bytes: want malformed, got %v", err)
	}
}
