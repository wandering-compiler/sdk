package dqlbind

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ScanTime is the sql.Scanner a generated scan reads a temporal column into
// when the destination is a google.protobuf.Timestamp:
// `rows.Scan(dqlbind.ScanTime(&createdAtTime))`.
//
// Drivers do not agree on the Go shape of a temporal column. DATE, DATETIME and
// TIMESTAMP arrive as time.Time, but a time-of-day column does not: the pgx
// database/sql driver hands a PostgreSQL TIME / TIMETZ over as its text form,
// MySQL sends TIME as bytes even with parseTime, and SQLite stores it as text.
// A plain `*time.Time` destination fails on every one of those rows, so the
// scan accepts all three shapes. A time of day is dated 0000-01-01 UTC (or in
// its own offset for TIMETZ). A NULL is an error: the generated query scans a
// NOT NULL column through here, and a nullable one through [ScanNullTime].
func ScanTime(dst *time.Time) sql.Scanner { return timeScanner{dst: dst} }

// ScanNullTime is [ScanTime] for a nullable column: a NULL leaves Valid false.
func ScanNullTime(dst *sql.NullTime) sql.Scanner { return nullTimeScanner{dst: dst} }

type timeScanner struct{ dst *time.Time }

func (s timeScanner) Scan(src any) error {
	if src == nil {
		return errors.New("dqlbind: NULL scanned into a NOT NULL time column")
	}
	t, err := parseSQLTime(src)
	if err != nil {
		return err
	}
	*s.dst = t
	return nil
}

type nullTimeScanner struct{ dst *sql.NullTime }

func (s nullTimeScanner) Scan(src any) error {
	if src == nil {
		*s.dst = sql.NullTime{}
		return nil
	}
	t, err := parseSQLTime(src)
	if err != nil {
		return err
	}
	*s.dst = sql.NullTime{Time: t, Valid: true}
	return nil
}

// sqlTimeLayouts are the text forms the three engines produce, most specific
// first. The time-of-day forms cover PostgreSQL TIME ("15:04:05[.ffffff]"),
// TIMETZ ("15:04:05+02", "15:04:05+05:30") and MySQL / SQLite TIME. The
// date-time forms cover a timestamp a driver returns as text (SQLite without a
// declared temporal type).
var sqlTimeLayouts = []string{
	"15:04:05.999999999Z07:00",
	"15:04:05.999999999Z07",
	"15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func parseSQLTime(src any) (time.Time, error) {
	var s string
	switch v := src.(type) {
	case time.Time:
		return v, nil
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return time.Time{}, fmt.Errorf("dqlbind: cannot scan %T into a time", src)
	}
	for _, layout := range sqlTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	// MySQL TIME is a duration that may be negative or past 24h
	// ("-01:00:00", "838:59:59"); neither is a time of day.
	return time.Time{}, fmt.Errorf("dqlbind: %q is not a time of day or a timestamp", s)
}
