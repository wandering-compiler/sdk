package runtime

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// tokenRE matches one `${...}` interpolation token. The body is any
// run of non-`}` characters.
var tokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// Expand walks a request-input value and resolves every `${...}`
// token — generators (`${random:uuid}`, `${seq}`) and capture
// references (`${name}`) — against the scope. Maps and slices are
// recursed; other scalars pass through unchanged. The result is a
// fresh structure safe to hand to the transport encoder.
func Expand(v any, s *Scope) (any, error) {
	switch t := v.(type) {
	case string:
		return s.expandString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ev, err := Expand(val, s)
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case map[any]any:
		m, ok := asStringMap(t)
		if !ok {
			return nil, fmt.Errorf("e2e runtime: non-string map key in input")
		}
		return Expand(m, s)
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			ev, err := Expand(val, s)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	default:
		return v, nil
	}
}

// expandString resolves the tokens in one string. When the whole
// string is exactly one token (`${expr}` with nothing around it) the
// resolved value is returned with its native type (so a captured int
// stays an int, a captured object stays an object). Otherwise each
// token is stringified and substituted in place (e.g.
// `user${seq}@example.com`).
func (s *Scope) expandString(str string) (any, error) {
	loc := tokenRE.FindStringSubmatchIndex(str)
	if loc == nil {
		return str, nil // no token
	}
	// whole-string single token?
	if loc[0] == 0 && loc[1] == len(str) {
		return s.resolve(str[loc[2]:loc[3]])
	}
	// embedded: stringify each token
	var firstErr error
	out := tokenRE.ReplaceAllStringFunc(str, func(tok string) string {
		expr := tok[2 : len(tok)-1] // strip ${ }
		val, err := s.resolve(expr)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return ""
		}
		return textOf(val)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// expandRegex interpolates a regex pattern: each `${…}` token is resolved
// like anywhere else and substituted as a LITERAL (regexp.QuoteMeta) — a
// captured value is data to find, so `1.2` must not match `1x2`. A `$`
// escaped with a backslash (`\${x}`, the usual way to mean those characters
// in a regex) is not a token.
func (s *Scope) expandRegex(pattern string) (string, error) {
	var b strings.Builder
	last := 0
	for _, loc := range regexTokens(pattern) {
		b.WriteString(pattern[last:loc[0]])
		val, err := s.resolve(pattern[loc[0]+2 : loc[1]-1])
		if err != nil {
			return "", err
		}
		b.WriteString(regexp.QuoteMeta(textOf(val)))
		last = loc[1]
	}
	b.WriteString(pattern[last:])
	return b.String(), nil
}

// regexTokens finds the `${…}` tokens of a regex pattern, skipping one whose
// `$` is backslash-escaped.
func regexTokens(pattern string) [][]int {
	var out [][]int
	for _, loc := range tokenRE.FindAllStringIndex(pattern, -1) {
		if loc[0] > 0 && pattern[loc[0]-1] == '\\' {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// textOf renders a resolved value for substitution into a string. A JSON
// number decodes as float64, and fmt.Sprint writes 1234567 as 1.234567e+06 —
// so a captured id embedded in `<ID>${id}</ID>` would never match. Integral
// values print as integers, the rest without an exponent.
func textOf(v any) string {
	if f, ok := v.(float64); ok {
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// resolve evaluates one token body: a generator (reserved `random:`
// / `seq` / `seq:<name>` prefixes) or a capture lookup (everything
// else). An unresolved capture is an error — a test referencing a
// value no prior step bound is a bug, not an empty string.
func (s *Scope) resolve(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	switch {
	case strings.HasPrefix(expr, "random:"):
		return generateRandom(strings.TrimPrefix(expr, "random:"))
	case expr == "seq":
		return s.run.nextSeq(""), nil
	case strings.HasPrefix(expr, "seq:"):
		return s.run.nextSeq(strings.TrimPrefix(expr, "seq:")), nil
	case strings.HasPrefix(expr, "once:"):
		// Mint on first use, then REUSE within this scope.
		//
		// `seq` and `seq:<name>` both climb on every resolution — naming one
		// partitions the counter, it does not share the value. So a generated
		// e-mail built in one step and looked up in another never matched:
		// `worker${seq}@…` produced 163 where the search produced 164, and the
		// failure reads as "no such user" rather than as two different values.
		//
		// The workaround is a hard-coded value, which is safe exactly once —
		// the second run of the same chain collides on a unique index. `once`
		// is the missing middle: still generated, still unique per scope, and
		// the SAME on both sides.
		//
		// Scope, deliberately, not the run: two top-level tests must not share
		// a generated identity, or one test's rows answer another's lookups
		// and the suite passes for the wrong reason.
		return s.once(strings.TrimPrefix(expr, "once:")), nil
	default:
		v, ok := s.lookup(expr)
		if !ok {
			return nil, fmt.Errorf("e2e runtime: unresolved reference ${%s} (no generator with that prefix, and no captured value bound)", expr)
		}
		return v, nil
	}
}
