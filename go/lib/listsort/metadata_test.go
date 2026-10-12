package listsort

import (
	"context"
	"strconv"
	"testing"

	"google.golang.org/grpc/metadata"
)

var columns = []string{"name", "email", "created_at"}

// A sort the column list does not know falls back to the default order.
func TestIndexForField_UnknownFieldIsTheDefaultOrder(t *testing.T) {
	for _, c := range []struct {
		field, dir string
		want       int64
	}{
		{"", "asc", 0},
		{"password_hash", "asc", 0},
		{"Email", "desc", 0}, // names match exactly
		{"name", "asc", 1},
		{"name", "desc", 2},
		{"created_at", "asc", 5},
		{"created_at", "desc", 6},
	} {
		if got := IndexForField(columns, c.field, c.dir); got != c.want {
			t.Errorf("IndexForField(%q, %q) = %d, want %d", c.field, c.dir, got, c.want)
		}
	}
}

// A header that is not a non-negative integer reads as no sort at all.
func TestSortByFromIncomingMD_MalformedOrNegativeIsTheDefault(t *testing.T) {
	read := func(v string) (int64, bool) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(SortByMDKey, v))
		return SortByFromIncomingMD(ctx)
	}
	for _, v := range []string{"", "abc", "-3", "2; DROP TABLE users", "email", "9999999999999999999999"} {
		if n, ok := read(v); n != 0 || ok {
			t.Errorf("header %q = (%d, %v), want (0, false)", v, n, ok)
		}
	}
	if n, ok := read("4"); n != 4 || !ok {
		t.Errorf("header \"4\" = (%d, %v), want (4, true)", n, ok)
	}
	if n, ok := SortByFromIncomingMD(context.Background()); n != 0 || ok {
		t.Errorf("no metadata = (%d, %v), want (0, false)", n, ok)
	}
}

// What travels is a number: no column name reaches the header, and the
// number the storage tier reads back is the one the gateway chose.
func TestEncodeSortByMD_OnlyAnIndexTravels(t *testing.T) {
	for i, field := range columns {
		for _, dir := range []string{"asc", "desc"} {
			v := EncodeSortByMD(IndexForField(columns, field, dir))
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("header for %s %s = %q, want a bare integer", field, dir, v)
			}
			want := IndexFor(i, dir == "desc")
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(SortByMDKey, v))
			if got, ok := SortByFromIncomingMD(ctx); !ok || got != want || n != want {
				t.Errorf("%s %s: sent %q, read (%d, %v), want %d", field, dir, v, got, ok, want)
			}
		}
	}
	for _, idx := range []int64{0, -1} {
		if v := EncodeSortByMD(idx); v != "" {
			t.Errorf("EncodeSortByMD(%d) = %q, want no header", idx, v)
		}
	}
}
