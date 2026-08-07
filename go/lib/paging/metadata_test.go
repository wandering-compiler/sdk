package paging_test

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestEncodeDecodeBoundariesMD_Roundtrip(t *testing.T) {
	bs := []*w17pb.KeysetValue{paging.FromInt64(42), paging.FromString("abc")}

	enc, err := paging.EncodeBoundariesMD(bs, w17pb.Direction_DIRECTION_FORWARD)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" {
		t.Fatal("encode: empty result")
	}

	dec, ok := paging.DecodeBoundariesMD(enc)
	if !ok {
		t.Fatal("decode: ok=false, want true")
	}
	if len(dec.Values) != 2 {
		t.Fatalf("decode: len=%d, want 2", len(dec.Values))
	}
	if dec.Values[0].GetIntV() != 42 {
		t.Errorf("decode[0]: got %v, want int 42", dec.Values[0])
	}
	if dec.Values[1].GetStringV() != "abc" {
		t.Errorf("decode[1]: got %v, want string abc", dec.Values[1])
	}
	if dec.Direction != w17pb.Direction_DIRECTION_FORWARD {
		t.Errorf("decode: direction=%v, want FORWARD", dec.Direction)
	}
}

func TestEncodeBoundariesMD_NilIsEmpty(t *testing.T) {
	enc, err := paging.EncodeBoundariesMD(nil, w17pb.Direction_DIRECTION_FORWARD)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if enc != "" {
		t.Errorf("encode nil: want empty, got %q", enc)
	}
}

func TestDecodeBoundariesMD_Empty(t *testing.T) {
	if _, ok := paging.DecodeBoundariesMD(""); ok {
		t.Error("decode empty: want ok=false")
	}
}

func TestDecodeBoundariesMD_Malformed(t *testing.T) {
	if _, ok := paging.DecodeBoundariesMD("not-valid-base64-_!@#"); ok {
		t.Error("decode malformed: want ok=false")
	}
}

// A crafted metadata header carrying a KeysetValue with an unset oneof
// (valid proto wire) must be rejected as malformed, not threaded through
// to emitted storage code where ScalarOf would panic on the empty oneof.
// Mirrors the client-facing cursor guard in DecodeCursor.
func TestDecodeBoundariesMD_EmptyOneofRejected(t *testing.T) {
	env := &w17pb.PageCursor{
		Boundaries: []*w17pb.KeysetValue{
			paging.FromInt64(1),
			{}, // unset oneof
		},
		Direction: w17pb.Direction_DIRECTION_FORWARD,
	}
	raw, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hdr := base64.RawURLEncoding.EncodeToString(raw)
	if _, ok := paging.DecodeBoundariesMD(hdr); ok {
		t.Error("decode empty-oneof boundary: want ok=false (malformed)")
	}
}

func TestBoundariesFromIncomingMD(t *testing.T) {
	bs := []*w17pb.KeysetValue{paging.FromInt64(99)}
	enc, err := paging.EncodeBoundariesMD(bs, w17pb.Direction_DIRECTION_BACKWARD)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	md := metadata.New(map[string]string{
		paging.BoundariesMDKey: enc,
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	got, ok := paging.BoundariesFromIncomingMD(ctx)
	if !ok {
		t.Fatal("FromIncomingMD: ok=false, want true")
	}
	if len(got.Values) != 1 || got.Values[0].GetIntV() != 99 {
		t.Errorf("FromIncomingMD: got %+v, want [99]", got.Values)
	}
	if got.Direction != w17pb.Direction_DIRECTION_BACKWARD {
		t.Errorf("FromIncomingMD: direction = %v, want BACKWARD", got.Direction)
	}
}

func TestBoundariesFromIncomingMD_NoMetadata(t *testing.T) {
	if _, ok := paging.BoundariesFromIncomingMD(context.Background()); ok {
		t.Error("FromIncomingMD on plain ctx: want ok=false")
	}
}

func TestFromContext_FallsBackToMD(t *testing.T) {
	// No context-value boundaries — should fall back to MD.
	bs := []*w17pb.KeysetValue{paging.FromInt64(7)}
	enc, _ := paging.EncodeBoundariesMD(bs, w17pb.Direction_DIRECTION_FORWARD)
	md := metadata.New(map[string]string{paging.BoundariesMDKey: enc})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	got, ok := paging.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok=false on MD-only path, want true")
	}
	if got.Values[0].GetIntV() != 7 {
		t.Errorf("FromContext: got %v, want [7]", got.Values)
	}
}

func TestEncodeLimitMD_AndDecodeRoundtrip(t *testing.T) {
	if got := paging.EncodeLimitMD(0); got != "" {
		t.Errorf("EncodeLimitMD(0) = %q, want empty", got)
	}
	if got := paging.EncodeLimitMD(42); got != "42" {
		t.Errorf("EncodeLimitMD(42) = %q, want 42", got)
	}

	md := metadata.New(map[string]string{paging.LimitMDKey: paging.EncodeLimitMD(123)})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	got, ok := paging.LimitFromIncomingMD(ctx)
	if !ok {
		t.Fatal("LimitFromIncomingMD: ok=false, want true")
	}
	if got != 123 {
		t.Errorf("LimitFromIncomingMD = %d, want 123", got)
	}
}

func TestLimitFromIncomingMD_Absent(t *testing.T) {
	if _, ok := paging.LimitFromIncomingMD(context.Background()); ok {
		t.Error("absent MD: want ok=false")
	}
}

func TestLimitFromIncomingMD_Malformed(t *testing.T) {
	md := metadata.New(map[string]string{paging.LimitMDKey: "not-a-number"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if _, ok := paging.LimitFromIncomingMD(ctx); ok {
		t.Error("malformed MD: want ok=false")
	}
}

func TestEncodeTotalMD_AndDecodeRoundtrip(t *testing.T) {
	if got := paging.EncodeTotalMD(0); got != "" {
		t.Errorf("EncodeTotalMD(0) = %q, want empty", got)
	}
	if got := paging.EncodeTotalMD(1247); got != "1247" {
		t.Errorf("EncodeTotalMD(1247) = %q, want 1247", got)
	}

	md := metadata.New(map[string]string{paging.TotalMDKey: paging.EncodeTotalMD(99999)})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	got, ok := paging.TotalFromIncomingMD(ctx)
	if !ok {
		t.Fatal("TotalFromIncomingMD: ok=false, want true")
	}
	if got != 99999 {
		t.Errorf("TotalFromIncomingMD = %d, want 99999", got)
	}
}

func TestTotalFromIncomingMD_Absent(t *testing.T) {
	if _, ok := paging.TotalFromIncomingMD(context.Background()); ok {
		t.Error("absent MD: want ok=false")
	}
}

func TestTotalFromIncomingMD_Malformed(t *testing.T) {
	md := metadata.New(map[string]string{paging.TotalMDKey: "garbage"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if _, ok := paging.TotalFromIncomingMD(ctx); ok {
		t.Error("malformed MD: want ok=false")
	}
}

func TestFromContext_CtxValueWinsOverMD(t *testing.T) {
	// Set both ctx value AND metadata; ctx value wins.
	mdBs := []*w17pb.KeysetValue{paging.FromInt64(7)}
	enc, _ := paging.EncodeBoundariesMD(mdBs, w17pb.Direction_DIRECTION_FORWARD)
	md := metadata.New(map[string]string{paging.BoundariesMDKey: enc})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ctxBs := &paging.Boundaries{
		Values:    []*w17pb.KeysetValue{paging.FromInt64(99)},
		Direction: w17pb.Direction_DIRECTION_BACKWARD,
	}
	ctx = paging.WithBoundaries(ctx, ctxBs)

	got, ok := paging.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok=false, want true")
	}
	if got.Values[0].GetIntV() != 99 {
		t.Errorf("FromContext: got %v, want ctx-value [99] (not MD [7])", got.Values)
	}
}
