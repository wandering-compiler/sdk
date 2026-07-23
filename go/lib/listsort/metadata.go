// Package listsort carries the admin auto-sort selector (item 2)
// between the gateway and the storage service over gRPC metadata —
// the runtime twin of `sdk/go/lib/paging` for column sort.
//
// The admin gateway reads the SPA's `?sort_by=<field>&sort_dir=<dir>`,
// maps the (field, direction) pair to a small integer index — the same
// index the storage codegen baked into its `CASE :__sort_by = <idx> …`
// ORDER BY — and forwards it via the [SortByMDKey] header. Storage's
// generated handler reads it with [SortByFromIncomingMD] into the
// `__sort_by` bind local. Absent / malformed header → 0 = "no explicit
// sort", so the list keeps its DQL-declared default order.
//
// Index encoding (shared with srcgo/domains/storage/generate/sort.go):
//
//	0        → no explicit sort (default order / tiebreak)
//	2*i + 1  → sortable column i, ASCending
//	2*i + 2  → sortable column i, DESCending
//
// where i is the 0-based position of the column in the admin list's
// sortable-column set. Keeping both sides on the same positional index
// means neither needs to ship column names on the wire.
package listsort

import (
	"context"
	"strconv"

	"google.golang.org/grpc/metadata"
)

// SortByMDKey is the gRPC metadata key carrying the sort selector index.
// The value is the index as a base-10 ASCII integer (no padding).
// Lowercase per gRPC convention.
const SortByMDKey = "x-w17-sort-by"

// EncodeSortByMD renders the sort selector index as the metadata header
// value. A zero index (no explicit sort) returns "" so the caller can
// skip the header entirely — an absent header reads back as 0, the same
// "default order" outcome.
func EncodeSortByMD(index int64) string {
	if index <= 0 {
		return ""
	}
	return strconv.FormatInt(index, 10)
}

// SortByFromIncomingMD reads the sort selector index from incoming gRPC
// metadata. Returns (0, false) when the header is absent OR malformed —
// storage's emitted ORDER BY then leaves `__sort_by` at 0 and the list
// falls back to its DQL-declared default order.
func SortByFromIncomingMD(ctx context.Context) (int64, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, false
	}
	vals := md.Get(SortByMDKey)
	if len(vals) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(vals[0], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// IndexFor maps a 0-based sortable-column position + direction to the
// wire index. descending selects the DESC arm. This is the single
// encoding both the gateway (threading the header) and any test helper
// should use so the storage-side `CASE :__sort_by = <idx>` matches.
func IndexFor(columnPos int, descending bool) int64 {
	base := int64(2*columnPos + 1)
	if descending {
		base++
	}
	return base
}

// IndexForField resolves a SPA sort request — the chosen column `field`
// and direction `dir` ("desc" descending, anything else ascending) —
// against the admin list's ordered `fields` (its sortable column set, in
// the same order the storage emit indexed them). Returns 0 when the
// field is empty or not in `fields` (⇒ default order); otherwise the wire
// index for the field's position + direction. The gateway calls this to
// turn `?sort_by=&sort_dir=` into the [SortByMDKey] header value.
func IndexForField(fields []string, field, dir string) int64 {
	if field == "" {
		return 0
	}
	for i, f := range fields {
		if f == field {
			return IndexFor(i, dir == "desc")
		}
	}
	return 0
}
