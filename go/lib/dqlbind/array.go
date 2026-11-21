package dqlbind

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// ArrayElem is an element type a PostgreSQL array column scans into: the Go
// scalar of a repeated proto field.
type ArrayElem interface {
	~string | ~[]byte | ~bool | ~int32 | ~int64 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// ScanArray is the sql.Scanner a generated PostgreSQL scan reads an array
// column into: `rows.Scan(dqlbind.ScanArray(&resp.Tags))`.
//
// The pgx database/sql driver hands an array column over in its text form
// (`{a,"b,c",NULL}`), so the scan parses that form once, straight into the
// destination slice — no intermediate value per element and no type map. A
// NULL array leaves the slice nil; a NULL element is an error, since a Go
// scalar has no NULL (the generated query never produces one: the array is
// built from NOT NULL values or COALESCEd). Only one-dimensional arrays are
// read; a repeated field is a flat list.
func ScanArray[T ArrayElem](dst *[]T) sql.Scanner { return &arrayScanner[T]{dst: dst} }

type arrayScanner[T ArrayElem] struct{ dst *[]T }

func (a *arrayScanner[T]) Scan(src any) error {
	var text string
	switch v := src.(type) {
	case nil:
		*a.dst = nil
		return nil
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("dqlbind: cannot scan %T into an array of %T", src, *new(T))
	}
	elems, err := parseArrayText(text)
	if err != nil {
		return err
	}
	out := make([]T, len(elems))
	for i, e := range elems {
		if e.null {
			return fmt.Errorf("dqlbind: array element %d is NULL, which %T cannot hold", i, out[i])
		}
		if err := parseElem(e.text, &out[i]); err != nil {
			return fmt.Errorf("dqlbind: array element %d: %w", i, err)
		}
	}
	*a.dst = out
	return nil
}

type arrayElemText struct {
	text string
	null bool
}

// parseArrayText splits the text form of a one-dimensional PostgreSQL array
// into its elements: `{` elements separated by `,` `}`, an element either
// bare (NULL when it is the word NULL) or double-quoted with backslash
// escapes. An optional dimension decoration (`[1:2]=`) is skipped.
func parseArrayText(s string) ([]arrayElemText, error) {
	if i := strings.IndexByte(s, '='); i > 0 && s[0] == '[' {
		s = s[i+1:]
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("dqlbind: %q is not an array", s)
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return []arrayElemText{}, nil
	}
	var out []arrayElemText
	for i := 0; ; {
		if i < len(body) && body[i] == '{' {
			return nil, fmt.Errorf("dqlbind: a multi-dimensional array does not scan into a list")
		}
		var e arrayElemText
		if i < len(body) && body[i] == '"' {
			var b strings.Builder
			i++
			for ; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' && i+1 < len(body) {
					i++
				}
				b.WriteByte(body[i])
			}
			if i >= len(body) {
				return nil, fmt.Errorf("dqlbind: unterminated quoted element in %q", s)
			}
			i++ // closing quote
			e.text = b.String()
		} else {
			j := i
			for j < len(body) && body[j] != ',' {
				j++
			}
			e.text = body[i:j]
			e.null = strings.EqualFold(e.text, "NULL")
			i = j
		}
		out = append(out, e)
		if i >= len(body) {
			return out, nil
		}
		if body[i] != ',' {
			return nil, fmt.Errorf("dqlbind: unexpected %q in array %q", body[i], s)
		}
		i++
	}
}

func parseElem[T ArrayElem](s string, dst *T) error {
	switch p := any(dst).(type) {
	case *string:
		*p = s
	case *[]byte:
		b, err := parseBytea(s)
		if err != nil {
			return err
		}
		*p = b
	case *bool:
		switch s {
		case "t", "true":
			*p = true
		case "f", "false":
			*p = false
		default:
			return fmt.Errorf("%q is not a boolean", s)
		}
	case *int32:
		n, err := strconv.ParseInt(s, 10, 32)
		*p = int32(n)
		return err
	case *int64:
		n, err := strconv.ParseInt(s, 10, 64)
		*p = n
		return err
	case *uint32:
		n, err := strconv.ParseUint(s, 10, 32)
		*p = uint32(n)
		return err
	case *uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		*p = n
		return err
	case *float32:
		f, err := parseFloat(s, 32)
		*p = float32(f)
		return err
	case *float64:
		f, err := parseFloat(s, 64)
		*p = f
		return err
	default:
		// A named type (an enum over int32, a string newtype): parse into
		// its underlying type and convert.
		return parseNamedElem(s, dst)
	}
	return nil
}

// parseNamedElem is the slow path for a named element type (an enum over
// int32, a string newtype): parse by the underlying kind and set through
// reflection. The exact scalar types never reach it.
func parseNamedElem[T ArrayElem](s string, dst *T) error {
	v := reflect.ValueOf(dst).Elem()
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		var b bool
		if err := parseElem(s, &b); err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := parseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.Slice: // a named []byte
		b, err := parseBytea(s)
		if err != nil {
			return err
		}
		v.SetBytes(b)
	default:
		return fmt.Errorf("element type %s does not scan from an array", v.Type())
	}
	return nil
}

func parseFloat(s string, bits int) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		return math.Inf(1), nil
	case "-Infinity":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, bits)
}

// parseBytea reads a bytea element in either output format: hex (`\x0102`,
// the default since PostgreSQL 9.0) or escape (octal `\001` and `\\`).
func parseBytea(s string) ([]byte, error) {
	if strings.HasPrefix(s, `\x`) {
		return hex.DecodeString(s[2:])
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\\' {
			out = append(out, '\\')
			i++
			continue
		}
		if i+3 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err != nil {
				return nil, fmt.Errorf("bad bytea escape in %q", s)
			}
			out = append(out, byte(n))
			i += 3
			continue
		}
		return nil, fmt.Errorf("bad bytea escape in %q", s)
	}
	return out, nil
}
