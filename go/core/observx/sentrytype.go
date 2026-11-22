package observx

import (
	"regexp"
	"strings"

	"github.com/getsentry/sentry-go"
)

// Sentry names an exception by its Go type (reflect.TypeOf(err)), so an
// error wrapped with fmt.Errorf("…: %w", err) — nearly every error a service
// reports — showed as `*fmt.wrapError`: the type of the wrapper, which says
// nothing to someone reading the issue, Go programmer or not. readableTypes
// renames each exception of the chain before the event is sent:
//
//   - a generic wrapper or a bare errors.New takes the type of the nearest
//     error it wraps that has a type of its own (`pgconn.PgError`);
//   - a gRPC status is named by its code (`Unavailable`, `Internal`);
//   - with neither, `Error`;
//   - any other type loses its leading `*` (`pgconn.PgError`).
//
// The values (the messages) are untouched.
func readableTypes(e *sentry.Event) *sentry.Event {
	if e == nil || len(e.Exception) == 0 {
		return e
	}
	// The chain is innermost first, outermost last (sentry-go reverses it).
	names := make([]string, len(e.Exception))
	for i, x := range e.Exception {
		names[i] = ownName(x)
	}
	inner := ""
	for i := range e.Exception {
		if names[i] != "" {
			inner = names[i]
		} else if inner != "" {
			names[i] = inner
		} else {
			names[i] = "Error"
		}
		e.Exception[i].Type = names[i]
	}
	return e
}

// genericTypes carry no meaning of their own: a wrapper, a joined error or
// a bare errors.New / fmt.Errorf without %w.
var genericTypes = map[string]bool{
	"*fmt.wrapError": true, "*fmt.wrapErrors": true, "*errors.errorString": true, "*errors.joinError": true,
}

var grpcCode = regexp.MustCompile(`^rpc error: code = ([A-Za-z]+) `)

// ownName is x's readable type, or "" when it is generic and takes the one
// of what it wraps.
func ownName(x sentry.Exception) string {
	switch {
	case genericTypes[x.Type]:
		return ""
	case x.Type == "*status.Error":
		if m := grpcCode.FindStringSubmatch(x.Value); m != nil {
			return m[1]
		}
		return "GRPCError"
	}
	return strings.TrimPrefix(x.Type, "*")
}
