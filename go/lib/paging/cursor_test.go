package paging_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testSchemaVersion = 0xDEADBEEF

// testKey is the HMAC key the cursor tests sign/verify with.
var testKey = []byte("test-cursor-hmac-key-0123456789ab")

// We exercise the encode/decode roundtrip against an
// arbitrary proto message — KeysetValue itself doubles as a
// stand-in for a real storage request, since this layer
// treats the request opaquely as bytes.
//
// (In production the request is a per-domain message like
// ListTasksReq; here we use w17.KeysetValue because it's
// guaranteed to be available without bootstrapping a domain
// fixture.)

func makeBoundaries() []*w17pb.KeysetValue {
	t, _ := time.Parse(time.RFC3339, "2026-05-19T12:00:00Z")
	return []*w17pb.KeysetValue{
		paging.FromTime(t),
		paging.FromInt64(42),
	}
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	req := paging.FromString("filter=open")
	boundaries := makeBoundaries()

	token, err := paging.EncodeCursor(req, boundaries, 1247, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if token == "" {
		t.Fatal("encode: empty token")
	}

	gotReq := &w17pb.KeysetValue{}
	gotB, gotTotal, gotDir, _, err := paging.DecodeCursor(token, gotReq, testSchemaVersion, testKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !proto.Equal(req, gotReq) {
		t.Errorf("request mismatch:\n  want: %v\n  got:  %v", req, gotReq)
	}
	if gotTotal != 1247 {
		t.Errorf("total: want 1247, got %d", gotTotal)
	}
	if gotDir != w17pb.Direction_DIRECTION_FORWARD {
		t.Errorf("direction: want FORWARD, got %v", gotDir)
	}
	if len(gotB) != len(boundaries) {
		t.Fatalf("boundaries length: want %d, got %d", len(boundaries), len(gotB))
	}
	for i := range boundaries {
		if !proto.Equal(boundaries[i], gotB[i]) {
			t.Errorf("boundary[%d] mismatch:\n  want: %v\n  got:  %v", i, boundaries[i], gotB[i])
		}
	}
}

// TestEncodeDecode_SortSelectorRoundtrip — INC-2 (admin auto-sort ×
// keyset paging). A paged + sortable list emits one keyset query variant
// per sort selector, each with its OWN ORDER BY, so the boundaries in a
// cursor are only meaningful under the variant that captured them. The
// selector therefore has to survive the round-trip verbatim — if it came
// back as 0 the Next hop would seek positional boundaries against the
// list's default order and hand back the wrong rows.
func TestEncodeDecode_SortSelectorRoundtrip(t *testing.T) {
	req := paging.FromString("filter=open")
	// 4 = sortable column 1, DESCending (2*i+2 with i=1) — a value that
	// is neither 0 nor 1, so a dropped or truncated field shows up.
	const sortBy = uint32(4)

	token, err := paging.EncodeCursor(req, makeBoundaries(), 5, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, sortBy, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got := &w17pb.KeysetValue{}
	_, _, _, gotSortBy, err := paging.DecodeCursor(token, got, testSchemaVersion, testKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotSortBy != sortBy {
		t.Errorf("sort selector: want %d, got %d", sortBy, gotSortBy)
	}
}

// TestEncodeDecode_SortSelectorIsNotSchemaVersion — the selector rides
// its own field, NOT the schema_version hash: a cursor minted under one
// sort variant must still DECODE (it is read back under its own variant),
// not be rejected as expired. Folding the selector into schema_version
// would strand every client that changed sort mid-session.
func TestEncodeDecode_SortSelectorIsNotSchemaVersion(t *testing.T) {
	req := paging.FromString("filter=open")
	token, err := paging.EncodeCursor(req, makeBoundaries(), 5, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 7, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := &w17pb.KeysetValue{}
	if _, _, _, _, err := paging.DecodeCursor(token, got, testSchemaVersion, testKey); err != nil {
		t.Fatalf("decode of a sorted cursor: want success, got %v", err)
	}
}

// R-bus-5: EncodeCursor's godoc promises the empty string when the
// cursor would be meaningless (no page edge to encode). With no
// boundary values it must return "" + nil — matching the doc and the
// generated gateway handlers' `token != ""` guard — not a bogus token.
func TestEncode_EmptyBoundaries_ReturnsEmpty(t *testing.T) {
	req := paging.FromString("filter=open")
	for _, b := range [][]*w17pb.KeysetValue{nil, {}} {
		token, err := paging.EncodeCursor(req, b, 1247, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
		if err != nil {
			t.Fatalf("encode with empty boundaries: unexpected error %v", err)
		}
		if token != "" {
			t.Errorf("encode with empty boundaries: token = %q, want \"\"", token)
		}
	}

	// Sanity: a non-empty boundary set still yields a token (the empty
	// case is the only short-circuit).
	token, err := paging.EncodeCursor(req, makeBoundaries(), 1, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode with boundaries: %v", err)
	}
	if token == "" {
		t.Error("encode with non-empty boundaries returned empty token")
	}
}

func TestSchemaVersion_Rejection(t *testing.T) {
	req := paging.FromString("filter=open")
	token, err := paging.EncodeCursor(req, makeBoundaries(), 0, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got := &w17pb.KeysetValue{}
	_, _, _, _, err = paging.DecodeCursor(token, got, testSchemaVersion+1, testKey)
	if !errors.Is(err, paging.ErrCursorExpired) {
		t.Fatalf("decode with wrong schema_version: want ErrCursorExpired, got %v", err)
	}
}

func TestDecode_MalformedBase64(t *testing.T) {
	got := &w17pb.KeysetValue{}
	_, _, _, _, err := paging.DecodeCursor("not-base64-url-_!@#", got, testSchemaVersion, testKey)
	if !errors.Is(err, paging.ErrCursorMalformed) {
		t.Fatalf("decode malformed base64: want ErrCursorMalformed, got %v", err)
	}
}

func TestDecode_EmptyToken(t *testing.T) {
	got := &w17pb.KeysetValue{}
	_, _, _, _, err := paging.DecodeCursor("", got, testSchemaVersion, testKey)
	if !errors.Is(err, paging.ErrCursorMalformed) {
		t.Fatalf("decode empty: want ErrCursorMalformed, got %v", err)
	}
}

func TestDecode_NotAPageCursor(t *testing.T) {
	// Raw base64 of arbitrary bytes — base64 decodes fine, but
	// proto unmarshal into PageCursor either fails or yields
	// an empty struct. Since proto unmarshal is permissive (it
	// will silently consume bytes that look like a valid proto),
	// we use bytes that definitely fail to parse.
	got := &w17pb.KeysetValue{}
	// A truncated varint head — proto wire format error.
	bogus := "____" // 3 bytes after base64-decode, but contains a partial wire-format prefix
	_, _, _, _, err := paging.DecodeCursor(bogus, got, testSchemaVersion, testKey)
	if err == nil {
		t.Fatalf("decode bogus payload: want error, got nil")
	}
	// Either malformed (fails base64 or proto parse) or expired
	// (parses as PageCursor with zero schema_version). Both are
	// acceptable rejections.
	if !errors.Is(err, paging.ErrCursorMalformed) && !errors.Is(err, paging.ErrCursorExpired) {
		t.Fatalf("decode bogus payload: want malformed or expired, got %v", err)
	}
}

// A cursor whose schema_version matches but carries a boundary with
// an unset KeysetValue oneof (valid proto wire) must decode to
// ErrCursorMalformed — NOT reach ScalarOf and panic (which would only
// be caught by the gateway's recover middleware as a 500).
func TestDecode_UnsetBoundaryOneof_Malformed(t *testing.T) {
	req := paging.FromString("filter=open")
	// One boundary with no oneof variant set.
	token, err := paging.EncodeCursor(req, []*w17pb.KeysetValue{{}}, 0, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := &w17pb.KeysetValue{}
	if _, _, _, _, err := paging.DecodeCursor(token, got, testSchemaVersion, testKey); !errors.Is(err, paging.ErrCursorMalformed) {
		t.Fatalf("decode unset-oneof boundary: want ErrCursorMalformed, got %v", err)
	}
}

func TestEncode_PreservesDirection(t *testing.T) {
	req := paging.FromString("x")
	token, err := paging.EncodeCursor(req, makeBoundaries(), 0, w17pb.Direction_DIRECTION_BACKWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := &w17pb.KeysetValue{}
	_, _, dir, _, err := paging.DecodeCursor(token, got, testSchemaVersion, testKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dir != w17pb.Direction_DIRECTION_BACKWARD {
		t.Errorf("direction: want BACKWARD, got %v", dir)
	}
}

// TestDecodeCursor_RejectsForgedCursor — Q34-gw-1. A client cannot
// craft a cursor (which DecodeCursor unmarshals OVER the whole request)
// to smuggle a request the server never minted: without the gateway's
// HMAC key it cannot produce a valid tag, so DecodeCursor rejects it
// before touching `req`. Models the IDOR attack: forge a PageCursor
// carrying an attacker-chosen request, sign it with the WRONG key,
// decode with the real key.
func TestDecodeCursor_RejectsForgedCursor(t *testing.T) {
	attackerKey := []byte("attacker-does-not-know-the-secret!")
	forgedReq := paging.FromString("project_id=victim-tenant-B")
	token, err := paging.EncodeCursor(forgedReq, makeBoundaries(), 1, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, attackerKey)
	if err != nil {
		t.Fatalf("encode (attacker): %v", err)
	}
	got := &w17pb.KeysetValue{}
	_, _, _, _, err = paging.DecodeCursor(token, got, testSchemaVersion, testKey)
	if !errors.Is(err, paging.ErrCursorMalformed) {
		t.Fatalf("forged cursor: want ErrCursorMalformed, got %v", err)
	}
	// The forged request must NOT have been unmarshalled into `got`.
	if proto.Equal(got, forgedReq) {
		t.Error("forged request leaked into req despite a bad MAC")
	}
}

// TestDecodeCursor_RejectsTamperedPayload — flipping any byte of the
// payload (the request/boundaries) invalidates the tag.
func TestDecodeCursor_RejectsTamperedPayload(t *testing.T) {
	token, err := paging.EncodeCursor(paging.FromString("ok"), makeBoundaries(), 1, w17pb.Direction_DIRECTION_FORWARD, testSchemaVersion, 0, testKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Mutate one character of the payload (before the '.'); the tag no
	// longer matches.
	b := []byte(token)
	dot := 0
	for i, c := range b {
		if c == '.' {
			dot = i
			break
		}
	}
	if dot == 0 {
		t.Fatal("token has no '.' separator")
	}
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	got := &w17pb.KeysetValue{}
	if _, _, _, _, err := paging.DecodeCursor(string(b), got, testSchemaVersion, testKey); !errors.Is(err, paging.ErrCursorMalformed) {
		t.Fatalf("tampered payload: want ErrCursorMalformed, got %v", err)
	}
}

// TestCursorKeyFromEnv — a set secret yields a stable sha256-derived
// key; an unset secret yields a random key + a warning. Distinct
// secrets yield distinct keys (so the env actually scopes the key).
func TestCursorKeyFromEnv(t *testing.T) {
	env := map[string]string{"APP_GATEWAY_CURSOR_SECRET": "s3cr3t"}
	getenv := func(k string) string { return env[k] }

	k1 := paging.CursorKeyFromEnv("APP_GATEWAY", getenv, nil)
	k2 := paging.CursorKeyFromEnv("APP_GATEWAY", getenv, nil)
	if len(k1) != 32 {
		t.Fatalf("derived key len = %d, want 32", len(k1))
	}
	if string(k1) != string(k2) {
		t.Error("set secret must yield a stable key across calls")
	}

	env["APP_GATEWAY_CURSOR_SECRET"] = "different"
	if string(paging.CursorKeyFromEnv("APP_GATEWAY", getenv, nil)) == string(k1) {
		t.Error("distinct secrets must yield distinct keys")
	}

	// Unset → random key + warning fired.
	warned := false
	rk := paging.CursorKeyFromEnv("APP_GATEWAY", func(string) string { return "" },
		func(string, ...any) { warned = true })
	if len(rk) != 32 {
		t.Errorf("random key len = %d, want 32", len(rk))
	}
	if !warned {
		t.Error("unset secret must warn via logf")
	}
}

func TestScalarOf_AllVariants(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		kv   *w17pb.KeysetValue
		want any
	}{
		{"int", paging.FromInt64(42), int64(42)},
		{"string", paging.FromString("hi"), "hi"},
		{"bool", paging.FromBool(true), true},
		{"float", paging.FromFloat64(3.14), 3.14},
		{"time", paging.FromTime(now), now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paging.ScalarOf(tc.kv)
			// time.Time has a special equality issue (location);
			// rely on Equal for that one.
			if gt, ok := got.(time.Time); ok {
				wt := tc.want.(time.Time)
				if !gt.Equal(wt) {
					t.Errorf("time scalar: want %v, got %v", wt, gt)
				}
				return
			}
			if got != tc.want {
				t.Errorf("scalar: want %v (%T), got %v (%T)", tc.want, tc.want, got, got)
			}
		})
	}
}

func TestScalarOf_Bytes(t *testing.T) {
	kv := paging.FromBytes([]byte{1, 2, 3})
	got := paging.ScalarOf(kv).([]byte)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("bytes scalar: want [1 2 3], got %v", got)
	}
}

func TestScalarOf_UnsetPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty oneof, got none")
		}
	}()
	paging.ScalarOf(&w17pb.KeysetValue{})
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		req, def, max, want uint32
	}{
		{0, 20, 100, 20},    // requested zero → default
		{50, 20, 100, 50},   // requested in range → requested
		{150, 20, 100, 100}, // over max → clamped
		{0, 50, 0, 50},      // max=0 → unlimited, but default applies
		{500, 50, 0, 500},   // max=0 → unlimited
	}
	for _, tc := range cases {
		got := paging.ClampLimit(tc.req, tc.def, tc.max)
		if got != tc.want {
			t.Errorf("ClampLimit(%d, def=%d, max=%d): want %d, got %d",
				tc.req, tc.def, tc.max, tc.want, got)
		}
	}
}

func TestTimestamppbInteropFreshness(t *testing.T) {
	// Sanity that timestamppb conversion preserves time.Time
	// to second precision. (Sub-second precision flows too,
	// but cursor stability across replicas is mostly
	// concerned with second-level ORDER BY columns.)
	now := time.Now().UTC().Round(time.Microsecond)
	ts := timestamppb.New(now)
	if got := ts.AsTime(); !got.Equal(now) {
		t.Fatalf("timestamppb roundtrip lost precision: want %v, got %v", now, got)
	}
}
