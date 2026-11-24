package dqlbind

import (
	"database/sql"
	"testing"
	"time"
)

func TestScanTime_DriverShapes(t *testing.T) {
	stamp := time.Date(2026, 8, 14, 12, 34, 56, 0, time.UTC)
	clock := time.Date(0, 1, 1, 12, 34, 56, 0, time.UTC)
	clockMicro := time.Date(0, 1, 1, 12, 34, 56, 123456000, time.UTC)
	cases := []struct {
		name string
		src  any
		want time.Time
	}{
		{"time.Time passes through", stamp, stamp},
		{"pgx TIME text", "12:34:56", clock},
		{"pgx TIME text with micros", "12:34:56.123456", clockMicro},
		{"mysql TIME bytes", []byte("12:34:56"), clock},
		{"pgx TIMETZ short offset", "12:34:56+02", time.Date(0, 1, 1, 12, 34, 56, 0, time.FixedZone("", 2*3600))},
		{"pgx TIMETZ long offset", "12:34:56+05:30", time.Date(0, 1, 1, 12, 34, 56, 0, time.FixedZone("", 5*3600+1800))},
		{"sqlite text timestamp", "2026-08-14 12:34:56", stamp},
		{"rfc3339 text", "2026-08-14T12:34:56Z", stamp},
		{"date text", "2026-08-14", time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got time.Time
			if err := ScanTime(&got).Scan(c.src); err != nil {
				t.Fatalf("Scan(%v): %v", c.src, err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("Scan(%v) = %v, want %v", c.src, got, c.want)
			}
			var n sql.NullTime
			if err := ScanNullTime(&n).Scan(c.src); err != nil || !n.Valid || !n.Time.Equal(c.want) {
				t.Fatalf("ScanNullTime(%v) = %+v, %v", c.src, n, err)
			}
		})
	}
}

func TestScanTime_Refusals(t *testing.T) {
	var got time.Time
	if err := ScanTime(&got).Scan(nil); err == nil {
		t.Fatal("NULL into a NOT NULL time scanned without an error")
	}
	for _, src := range []any{"838:59:59", "-01:00:00", "noon", int64(5)} {
		if err := ScanTime(&got).Scan(src); err == nil {
			t.Fatalf("Scan(%v) accepted a value that is no time", src)
		}
	}
	n := sql.NullTime{Time: time.Now(), Valid: true}
	if err := ScanNullTime(&n).Scan(nil); err != nil || n.Valid {
		t.Fatalf("ScanNullTime(nil) = %+v, %v; want invalid", n, err)
	}
}
