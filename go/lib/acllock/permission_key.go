package acllock

import "strings"

// PermissionKey converts a cascade permission CODE into the key a lock is
// actually allocated under.
//
// The ACL cascade names permissions as `<module>.<Model>#<action>` and
// `<module>.<Service>.<Method>`. The allocation crosses a proto enum on its way
// to disk, where a code cannot survive as an identifier, so every emitted lock
// is keyed by the enum VALUE NAME instead — `tasks.Task#view` is stored as
// `TASKS_TASK_VIEW`.
//
// Until T2-5 pass #10 this conversion existed only inside the compiler, while
// the public package documented the code form as the key and offered
// IDByString for "typical JWT claim shape". The two alphabets are disjoint, so
// the documented lookup always missed — fail-closed, but a legitimate grant
// dropped with nothing to point at. An auth backend translating claims needs
// the conversion, so it belongs here.
//
// The rule, mirroring the compiler's emitter:
//
//  1. `.` and `#` become `_`
//  2. camelCase boundaries become `_`
//  3. the whole string is upper-cased
//
// It is deterministic but lossy — `foo.bar` and `foo_bar` converge. The
// emitter detects such a collision and refuses, so a lock never contains one;
// a caller converting an arbitrary string should not assume injectivity.
//
// A string already in the enum-name alphabet passes through unchanged, which
// is what lets IDByString accept either form.
func PermissionKey(code string) string {
	var b strings.Builder
	b.Grow(len(code) + 8)
	prevLower := false
	for _, r := range code {
		switch {
		case r == '.' || r == '#':
			if b.Len() > 0 && !endsWithUnderscore(&b) {
				b.WriteByte('_')
			}
			prevLower = false
		case r >= 'A' && r <= 'Z':
			if prevLower && !endsWithUnderscore(&b) {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevLower = false
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32) // upcase
			prevLower = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = false
		default:
			// Anything else (rare; e.g. `-`) maps to `_` so the result stays a
			// valid proto identifier.
			if b.Len() > 0 && !endsWithUnderscore(&b) {
				b.WriteByte('_')
			}
			prevLower = false
		}
	}
	return b.String()
}

func endsWithUnderscore(b *strings.Builder) bool {
	s := b.String()
	return len(s) > 0 && s[len(s)-1] == '_'
}
