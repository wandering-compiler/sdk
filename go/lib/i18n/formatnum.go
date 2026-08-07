package i18n

import (
	"strconv"
	"strings"
)

// This file formats NUMBERS. It deliberately never touches float64 on the
// formatting path: every step is decimal-string arithmetic, because a value
// that reaches an admin cell can be an int64 beyond 2^53 (quoted on the JSON
// wire per docs/specs/gateway/json-dialect.md) or a decimal string, and
// routing those through a float would round them silently.
//
// Two conventions are FIXED here because Go and TS must agree byte for byte
// (docs/specs/i18n/formatting.md, "an explicit format table, not Intl"):
//
//   - rounding is HALF-UP away from zero: 2.345 → 2.35, -2.345 → -2.35.
//     Not half-even. Pick one and pin it, since the platforms disagree.
//   - the sign is ASCII hyphen-minus, never U+2212, and a value that rounds
//     to zero loses it: -0.004 at 2 places is "0.00", not "-0.00".

// FormatNumber renders a value with grouping and the locale's decimal
// separator at NATURAL precision: the digits the value actually carries, with
// trailing fraction zeros dropped ("1234.50" → "1 234,5", "1234.00" →
// "1 234"). This is the `number` filter.
//
// A value that is not a plain decimal (an empty string, "abc", exponent
// notation) is returned UNCHANGED. A malformed cell must render as itself
// rather than as a lie or a crash — same philosophy as the admin's
// displayString.
func FormatNumber(value string, f Formats) string {
	d, ok := parseDecimal(value)
	if !ok {
		return value
	}
	return renderDecimal(trimFractionZeros(d), f)
}

// FormatDecimal renders a value at exactly `places` decimal places, padding
// with zeros and rounding half-up as needed. This is the `decimal:N` filter,
// and the preset for MONEY (2) and DECIMAL (the field's declared scale).
//
// Negative `places` means natural precision, i.e. the same as [FormatNumber].
func FormatDecimal(value string, places int, f Formats) string {
	d, ok := parseDecimal(value)
	if !ok {
		return value
	}
	if places < 0 {
		return renderDecimal(trimFractionZeros(d), f)
	}
	return renderDecimal(roundDecimal(d, places), f)
}

// FormatPercent renders a value as [FormatDecimal] wrapped in the locale's
// percent shape — `{value} %` in Czech, `{value}%` in English. The value is
// NOT multiplied: a PERCENTAGE field carries the percentage, not the fraction.
func FormatPercent(value string, places int, f Formats) string {
	num := FormatDecimal(value, places, f)
	shape := f.PercentFormat
	if shape == "" {
		return num
	}
	return strings.ReplaceAll(shape, "{value}", num)
}

// DecimalString renders a float64 as the plain decimal string the formatters
// above take: shortest representation that round-trips, and NEVER exponent
// notation (which the parser rejects). It is the adapter for a MONEY field,
// which is a bare `double` carrier per proto/w17/field.proto.
//
// The TS mirror (`decimalString`) has to expand JS's exponent notation to
// reach the same digits; the shared golden vectors carry float inputs for
// exactly that reason.
func DecimalString(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// decimalParts is a parsed decimal: a sign, integer digits (never empty), and
// fraction digits (possibly empty). Digits only — no sign, no separator.
type decimalParts struct {
	neg   bool
	ipart string
	fpart string
}

// parseDecimal accepts the plain decimal grammar and nothing else:
// an optional sign, then digits with at most one dot. Exponent notation is
// rejected on purpose — it cannot appear in a w17 JSON payload (protojson
// emits plain decimals, DecimalString never produces one), so accepting it
// would only add a second rounding path.
func parseDecimal(s string) (decimalParts, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimalParts{}, false
	}
	var d decimalParts
	switch s[0] {
	case '-':
		d.neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	ip, fp, hasDot := strings.Cut(s, ".")
	if !allDigits(ip) || !allDigits(fp) {
		return decimalParts{}, false
	}
	if ip == "" && fp == "" {
		return decimalParts{}, false // "", ".", "-." are not numbers
	}
	if hasDot && fp == "" {
		// "5." — a trailing dot with no digits is not a decimal we emit.
		return decimalParts{}, false
	}
	if ip == "" {
		ip = "0" // ".5" → 0.5
	}
	d.ipart, d.fpart = ip, fp
	return d, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// roundDecimal rounds to `places` fraction digits, half-up away from zero,
// padding with zeros when the value is shorter than requested.
func roundDecimal(d decimalParts, places int) decimalParts {
	if len(d.fpart) <= places {
		d.fpart += strings.Repeat("0", places-len(d.fpart))
		return d
	}
	kept := d.ipart + d.fpart[:places]
	if d.fpart[places] >= '5' {
		kept = incrementDigits(kept)
	}
	d.ipart, d.fpart = kept[:len(kept)-places], kept[len(kept)-places:]
	return d
}

// incrementDigits adds one to a digit string, growing it on overflow
// ("999" → "1000"). Digits only; the caller owns the sign.
func incrementDigits(s string) string {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != '9' {
			b[i]++
			return string(b)
		}
		b[i] = '0'
	}
	return "1" + string(b)
}

// trimFractionZeros drops trailing zeros from the fraction — the natural
// precision the `number` filter renders.
func trimFractionZeros(d decimalParts) decimalParts {
	d.fpart = strings.TrimRight(d.fpart, "0")
	return d
}

// renderDecimal assembles the final string: sign, grouped integer digits, and
// the fraction behind the locale's decimal separator. A value whose digits are
// all zero renders unsigned.
func renderDecimal(d decimalParts, f Formats) string {
	var b strings.Builder
	if d.neg && !isZeroDigits(d.ipart, d.fpart) {
		b.WriteByte('-')
	}
	b.WriteString(groupDigits(d.ipart, f.Grouping, f.ThousandSeparator))
	if d.fpart != "" {
		b.WriteString(f.DecimalSeparator)
		b.WriteString(d.fpart)
	}
	return b.String()
}

func isZeroDigits(parts ...string) bool {
	for _, p := range parts {
		if strings.Trim(p, "0") != "" {
			return false
		}
	}
	return true
}

// groupDigits inserts `sep` every `size` digits from the right. A non-positive
// size, or an empty separator, means no grouping.
func groupDigits(digits string, size int, sep string) string {
	if size <= 0 || sep == "" || len(digits) <= size {
		return digits
	}
	lead := len(digits) % size
	var b strings.Builder
	b.Grow(len(digits) + (len(digits)/size)*len(sep))
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += size {
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(digits[i : i+size])
	}
	return b.String()
}
