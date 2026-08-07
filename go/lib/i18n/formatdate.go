package i18n

import (
	"strconv"
	"strings"
	"time"
)

// This file formats DATES and TIMES against the frozen table's patterns.
//
// Everything renders in UTC (docs/specs/i18n/formatting.md — per-user time
// zones are parked), so a value carrying an offset is converted first:
// `2026-07-26T23:30:00+02:00` is 21:30 UTC and renders as such.
//
// The token vocabulary is CLOSED and shared with the TS mirror:
//
//	YYYY  4-digit year        MM  2-digit month   M  month
//	DD    2-digit day         D   day
//	HH    2-digit hour 0-23   H   hour 0-23
//	hh    2-digit hour 1-12   h   hour 1-12       A  AM / PM
//	mm    2-digit minute      ss  2-digit second
//
// Anything else in a pattern is a literal. Since the table is frozen, no
// project-authored pattern ever reaches this renderer.

// FormatDate renders a temporal value with the locale's `date_format`.
// A value that parses as none of the accepted forms is returned unchanged —
// a malformed cell renders as itself rather than as a wrong date.
func FormatDate(value string, f Formats) string {
	return formatTemporal(value, f.DateFormat, value)
}

// FormatShortDate renders with `short_date_format` — the dense-table form
// (`26.7.2026` where the detail page shows `26.07.2026`).
func FormatShortDate(value string, f Formats) string {
	return formatTemporal(value, f.ShortDateFormat, value)
}

// FormatTime renders with `time_format` — 24-hour in most locales, `h:mm A`
// in en-US.
func FormatTime(value string, f Formats) string {
	return formatTemporal(value, f.TimeFormat, value)
}

// FormatDateTime renders with `datetime_format`, whose `{date}` and `{time}`
// placeholders are filled from `date_format` and `time_format`. Keeping the
// composition in the table (rather than hardcoding "date space time") is what
// lets a locale put the time first, or separate the two with a comma.
func FormatDateTime(value string, f Formats) string {
	c, ok := parseTemporal(value)
	if !ok {
		return value
	}
	shape := f.DateTimeFormat
	if shape == "" {
		shape = "{date} {time}"
	}
	out := strings.ReplaceAll(shape, "{date}", RenderTimePattern(f.DateFormat, c))
	return strings.ReplaceAll(out, "{time}", RenderTimePattern(f.TimeFormat, c))
}

// formatTemporal parses then renders, falling back to `orElse` when the value
// is not a temporal at all.
func formatTemporal(value, pattern, orElse string) string {
	c, ok := parseTemporal(value)
	if !ok {
		return orElse
	}
	return RenderTimePattern(pattern, c)
}

// CivilTime is a wall-clock instant in UTC, already stripped of zone. It is
// what the renderer walks; nothing downstream re-interprets it.
type CivilTime struct {
	Year, Month, Day     int
	Hour, Minute, Second int
}

// temporalLayouts are tried in order. The first four cover what a w17 payload
// actually carries (protojson emits RFC 3339 with seconds); the naive and
// date/time-only forms are accepted so a DATE or TIME column — which has no
// zone to carry — formats too.
var temporalLayouts = []string{
	time.RFC3339, // 2026-07-26T14:03:00Z / +02:00
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04Z07:00", // no seconds
	"2006-01-02T15:04:05",    // naive: already UTC by contract
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"15:04:05",
	"15:04",
}

// parseTemporal accepts the forms above and converts to UTC. A value with no
// zone is taken as UTC already; a date-only or time-only value fills the
// missing half with zeros, so formatting a DATE with a time pattern yields
// `0:00` rather than an error.
func parseTemporal(value string) (CivilTime, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return CivilTime{}, false
	}
	for _, layout := range temporalLayouts {
		t, err := time.Parse(layout, v)
		if err != nil {
			continue
		}
		t = t.UTC()
		return CivilTime{
			Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
			Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
		}, true
	}
	return CivilTime{}, false
}

// RenderTimePattern substitutes the closed token vocabulary documented at the
// top of this file. Tokens are matched longest-first so `YYYY` wins over a
// bare `Y`-less scan and `MM` over `M`; every other byte is copied verbatim.
func RenderTimePattern(pattern string, c CivilTime) string {
	var b strings.Builder
	b.Grow(len(pattern) + 8)
	for i := 0; i < len(pattern); {
		tok, val := matchToken(pattern[i:], c)
		if tok == 0 {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		b.WriteString(val)
		i += tok
	}
	return b.String()
}

// matchToken returns the length of the token at the head of `s` and its
// rendered value, or (0, "") when `s` does not start with a token.
func matchToken(s string, c CivilTime) (int, string) {
	switch {
	case strings.HasPrefix(s, "YYYY"):
		return 4, pad4(c.Year)
	case strings.HasPrefix(s, "MM"):
		return 2, pad2(c.Month)
	case strings.HasPrefix(s, "DD"):
		return 2, pad2(c.Day)
	case strings.HasPrefix(s, "HH"):
		return 2, pad2(c.Hour)
	case strings.HasPrefix(s, "hh"):
		return 2, pad2(hour12(c.Hour))
	case strings.HasPrefix(s, "mm"):
		return 2, pad2(c.Minute)
	case strings.HasPrefix(s, "ss"):
		return 2, pad2(c.Second)
	case strings.HasPrefix(s, "M"):
		return 1, strconv.Itoa(c.Month)
	case strings.HasPrefix(s, "D"):
		return 1, strconv.Itoa(c.Day)
	case strings.HasPrefix(s, "H"):
		return 1, strconv.Itoa(c.Hour)
	case strings.HasPrefix(s, "h"):
		return 1, strconv.Itoa(hour12(c.Hour))
	case strings.HasPrefix(s, "A"):
		if c.Hour < 12 {
			return 1, "AM"
		}
		return 1, "PM"
	}
	return 0, ""
}

// hour12 maps a 0-23 hour onto the 1-12 clock: midnight and noon are both 12.
func hour12(h int) int {
	switch {
	case h == 0:
		return 12
	case h > 12:
		return h - 12
	default:
		return h
	}
}

func pad2(v int) string {
	if v >= 0 && v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

func pad4(v int) string {
	s := strconv.Itoa(v)
	if v >= 0 && len(s) < 4 {
		return strings.Repeat("0", 4-len(s)) + s
	}
	return s
}
