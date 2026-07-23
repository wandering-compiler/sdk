package kvfs

import "testing"

// TestFormatBytes covers every unit branch of the diag-side byte
// renderer: the invariant is that a count is rendered in the
// largest unit that keeps the mantissa >= 1, with two decimals for
// scaled units and a bare "<n> B" below 1 KiB.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		name string
		in   uint64
		want string
	}{
		{name: "zero", in: 0, want: "0 B"},
		{name: "bytes", in: 512, want: "512 B"},
		{name: "just-under-kib", in: 1023, want: "1023 B"},
		{name: "one-kib", in: 1024, want: "1.00 KiB"},
		{name: "kib-fraction", in: 1536, want: "1.50 KiB"},
		{name: "one-mib", in: 1024 * 1024, want: "1.00 MiB"},
		{name: "mib-fraction", in: 1024*1024 + 512*1024, want: "1.50 MiB"},
		{name: "one-gib", in: 1024 * 1024 * 1024, want: "1.00 GiB"},
		{name: "gib-fraction", in: 3 * 1024 * 1024 * 1024 / 2, want: "1.50 GiB"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := formatBytes(tc.in); got != tc.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
