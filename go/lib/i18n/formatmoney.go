package i18n

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file formats MONEY. A w17 MONEY value is an int64 count of MINOR units
// (cents, haléře) — never a float — with a SCALE that says how many of those
// make one major unit: scale 2 means 12345 is 123.45. Everything below is the
// same decimal-string arithmetic formatnum.go uses, so the conventions there
// hold here too: rounding is half-up away from zero, the sign is ASCII
// hyphen-minus, and a value that rounds to zero is unsigned.

// Currency is one row of the frozen currencies table in formats.json: an ISO
// 4217 code, the locale-independent symbol CLDR gives it (`US$`, `CA$`, `€`;
// empty when CLDR has none, and the code itself is shown), and the number of
// decimals an amount in it is shown with (CLDR's digits: JPY 0, KWD 3, most 2).
type Currency struct {
	Code     string `json:"-"`
	Symbol   string `json:"symbol,omitempty"`
	Decimals int    `json:"decimals"`
}

// NormalizeCurrencyCode is how a code is compared: trimmed, with ASCII letters
// uppercased. `czk` and ` CZK ` are both CZK. Only ASCII is touched, because
// the TS mirror must agree byte for byte and the two languages' full Unicode
// case mappings do not.
func NormalizeCurrencyCode(code string) string {
	code = strings.TrimSpace(code)
	b := []byte(code)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// LookupCurrency returns the table row for a code (normalized first), and
// whether the table knows it.
func LookupCurrency(code string) (Currency, bool) {
	c, ok := currencyTable[NormalizeCurrencyCode(code)]
	return c, ok
}

// CurrencyCodes lists every code the table carries, sorted. The compiler
// validates a declared `currency` against this set.
func CurrencyCodes() []string {
	out := make([]string, 0, len(currencyTable))
	for code := range currencyTable {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

func isCurrencyCodeShape(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

// FormatMoney renders an amount of MINOR units as money. This is the `money`
// filter, and the preset of the MONEY type.
//
//  1. The value is shifted by `scale` — a decimal-string operation, so 12345
//     at scale 2 is exactly 123.45 and an int64 past 2^53 keeps every digit.
//  2. With no currency, that is the result: the scaled number at `scale`
//     decimals, grouped, without a symbol.
//  3. With one, the amount is rounded half-up to the CURRENCY's decimals (a
//     unit price kept at scale 4 shows as 2 decimals in CZK and 0 in JPY) and
//     placed into the locale's currency_format with the currency's symbol: the
//     locale's own spelling if it has one, else the table's, else the code.
//     A code the table does not know renders as itself, at `scale` decimals.
//
// A value that is not a plain decimal is returned UNCHANGED, as every formatter
// here does: a malformed cell renders as itself rather than as a lie.
//
// A symbol that is a word (`CZK`, `CHF`) is never glued to the digits: where
// the shape puts the two side by side, a space goes between them, as CLDR's
// currency spacing rule does — `CZK 1,234.50`, not `CZK1,234.50`.
func FormatMoney(value string, scale int, currency string, f Formats) string {
	d, ok := parseDecimal(value)
	if !ok {
		return value
	}
	if scale < 0 {
		scale = 0
	}
	d = shiftDecimal(d, scale)
	code := NormalizeCurrencyCode(currency)
	if code == "" {
		return renderDecimal(roundDecimal(d, scale), f)
	}
	places, symbol := scale, code
	if c, known := currencyTable[code]; known {
		places = c.Decimals
		if c.Symbol != "" {
			symbol = c.Symbol
		}
	}
	if s := f.CurrencySymbols[code]; s != "" {
		symbol = s
	}
	d = roundDecimal(d, places)
	neg := d.neg && !isZeroDigits(d.ipart, d.fpart)
	d.neg = false
	shape := f.CurrencyFormat
	if shape == "" {
		shape = "{value} {symbol}"
	}
	shape = spaceWordSymbol(shape, symbol)
	out := strings.NewReplacer("{value}", renderDecimal(d, f), "{symbol}", symbol).Replace(shape)
	if neg {
		return "-" + out
	}
	return out
}

// shiftDecimal divides by 10^scale by moving the decimal point, and drops the
// leading zeros the move leaves on the integer part (keeping one).
func shiftDecimal(d decimalParts, scale int) decimalParts {
	if scale == 0 {
		return d
	}
	digits := d.ipart + d.fpart
	point := len(d.ipart) - scale
	if point <= 0 {
		d.ipart, d.fpart = "0", strings.Repeat("0", -point)+digits
	} else {
		d.ipart, d.fpart = digits[:point], digits[point:]
	}
	d.ipart = strings.TrimLeft(d.ipart, "0")
	if d.ipart == "" {
		d.ipart = "0"
	}
	return d
}

// spaceWordSymbol inserts a space between `{symbol}` and `{value}` where the
// shape glues them and the symbol's touching end is a letter.
func spaceWordSymbol(shape, symbol string) string {
	if symbol == "" {
		return shape
	}
	if last, _ := utf8.DecodeLastRuneInString(symbol); unicode.IsLetter(last) {
		shape = strings.ReplaceAll(shape, "{symbol}{value}", "{symbol} {value}")
	}
	if first, _ := utf8.DecodeRuneInString(symbol); unicode.IsLetter(first) {
		shape = strings.ReplaceAll(shape, "{value}{symbol}", "{value} {symbol}")
	}
	return shape
}
