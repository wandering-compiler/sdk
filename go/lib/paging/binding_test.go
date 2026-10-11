package paging_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

var bindKey = []byte("0123456789abcdef0123456789abcdef")

func boundary() []*w17pb.KeysetValue {
	return []*w17pb.KeysetValue{{Value: &w17pb.KeysetValue_IntV{IntV: 7}}}
}

// A cursor bound to one caller decodes for that caller and for nobody else:
// a leaked cursor does not resume the minter's request for another user.
func TestCallerKey_ACursorDoesNotDecodeForAnotherCaller(t *testing.T) {
	alice := paging.CallerKey(bindKey, "user-a", "org-1")
	tok, err := paging.EncodeCursor(wrapperspb.String("filter=mine"), boundary(), 3, w17pb.Direction_DIRECTION_FORWARD, 1, 0, alice)
	if err != nil || tok == "" {
		t.Fatalf("encode: %q, %v", tok, err)
	}

	var req wrapperspb.StringValue
	if _, _, _, _, err := paging.DecodeCursor(tok, &req, 1, paging.CallerKey(bindKey, "user-a", "org-1")); err != nil {
		t.Fatalf("the minting caller cannot resume: %v", err)
	}
	for name, key := range map[string][]byte{
		"another user":           paging.CallerKey(bindKey, "user-b", "org-1"),
		"same user, another org": paging.CallerKey(bindKey, "user-a", "org-2"),
		"parts shifted":          paging.CallerKey(bindKey, "user-ao", "rg-1"),
		"unbound deployment key": bindKey,
		"no caller":              paging.CallerKey(bindKey),
	} {
		var other wrapperspb.StringValue
		_, _, _, _, err := paging.DecodeCursor(tok, &other, 1, key)
		if !errors.Is(err, paging.ErrCursorMalformed) {
			t.Errorf("%s: decode = %v, want ErrCursorMalformed", name, err)
		}
		if other.GetValue() != "" {
			t.Errorf("%s: the minter's request reached the caller's: %q", name, other.GetValue())
		}
	}
}

// Resuming REPLACES the request with the minted one: a filter the client
// re-sent alongside the cursor does not survive.
func TestDecodeCursor_ResumeReplacesTheRequest(t *testing.T) {
	tok, err := paging.EncodeCursor(wrapperspb.String("minted"), boundary(), 0, w17pb.Direction_DIRECTION_FORWARD, 1, 0, bindKey)
	if err != nil {
		t.Fatal(err)
	}
	req := wrapperspb.String("changed mid-session")
	if _, _, _, _, err := paging.DecodeCursor(tok, req, 1, bindKey); err != nil {
		t.Fatal(err)
	}
	if req.GetValue() != "minted" {
		t.Errorf("request after resume = %q, want the minted one", req.GetValue())
	}
}

// Storage does not take a limit header at its word: a value past MaxLimit is
// clamped, and zero is no limit header at all.
func TestLimitFromIncomingMD_ClampsToMaxLimit(t *testing.T) {
	for raw, want := range map[string]struct {
		n  uint32
		ok bool
	}{
		"4294967295": {paging.MaxLimit, true},
		"10001":      {paging.MaxLimit, true},
		"10000":      {10000, true},
		"25":         {25, true},
		"0":          {0, false},
	} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(paging.LimitMDKey, raw))
		n, ok := paging.LimitFromIncomingMD(ctx)
		if n != want.n || ok != want.ok {
			t.Errorf("header %s → (%d, %v), want (%d, %v)", raw, n, ok, want.n, want.ok)
		}
	}
}
