// REV-149 — HTTP→gRPC metadata RENAMING + default-value seed
// stamping. Sibling of [MetadataPropagationMiddleware] which
// forwards headers verbatim under a lower-cased key. The
// renaming middleware maps each HTTP header to a different
// metadata key (e.g. Accept-Language → w17-language); the
// default-stamp middleware seeds a metadata key with a fallback
// value only when nothing upstream has already populated it.
//
// Both middlewares are pass-through when their config is empty
// — operators who don't opt into rename / default seed pay
// nothing.
//
// Ordering convention (gateway main template wires them in
// this order between MetadataPropagationMiddleware and the
// router):
//
//   MetadataPropagation → MetadataRename → MetadataDefaultStamp → router
//
// So consumer-side last-value-wins ordering ends up:
//
//   verbatim header forward (lowest precedence)
//   → rename (overwrites verbatim when both target same key)
//   → handler-level metadata_bindings emit (REV-149 — per-endpoint,
//     overwrites all upstream values when fired)
//   → default stamp ONLY when nothing above wrote the key
//
// Consumers reading the metadata key should pick the LAST
// value of `md.Get(key)` to honor this precedence; small
// helpers (e.g. `i18n.MetadataLast`) do this for their own
// keys.

package restgw

import (
	"net/http"

	"google.golang.org/grpc/metadata"
)

// HeaderRenameRule is one HTTP header → gRPC metadata key
// renaming entry. Parser resolves both author-declared and
// auto-installed (e.g. Accept-Language → w17-language) rules
// into a flat list; the generator emits the list into the
// middleware config.
type HeaderRenameRule struct {
	// HTTP is the HTTP header name (case-insensitive match
	// via http.Header semantics).
	HTTP string

	// Metadata is the target gRPC metadata key (lowercase
	// ASCII per gRPC convention).
	Metadata string
}

// MetadataRenameMiddleware appends the matching HTTP header
// value to the outgoing gRPC metadata under the rule's target
// key. Multiple rules may map distinct HTTP headers to the
// same metadata key — the last present header's value lands
// last (consumer-side last-wins).
//
// Empty rules list returns `next` unchanged — operators
// without rename rules pay nothing.
//
// Headers absent / blank on the incoming request are skipped
// (the middleware never injects empty metadata values).
func MetadataRenameMiddleware(rules []HeaderRenameRule, next http.Handler) http.Handler {
	if len(rules) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pairs []string
		for _, rule := range rules {
			v := r.Header.Get(rule.HTTP)
			if v == "" {
				continue
			}
			pairs = append(pairs, rule.Metadata, v)
		}
		if len(pairs) > 0 {
			ctx := metadata.AppendToOutgoingContext(r.Context(), pairs...)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// DefaultMetadataStamp is one (key, value) fallback entry —
// the value lands on the outgoing gRPC metadata under `Key`
// only when no upstream middleware has already populated that
// key.
type DefaultMetadataStamp struct {
	// Key is the target gRPC metadata key (lowercase ASCII).
	Key string

	// Value is the default value to stamp when Key is absent
	// from the outgoing metadata.
	Value string
}

// MetadataDefaultStampMiddleware seeds the outgoing gRPC
// metadata with a per-key fallback value when that key has not
// been populated by upstream middleware. Used for surface-
// level defaults like `w17-language: <RestApi.default_language>`
// — the request's Accept-Language → w17-language rename (or an
// explicit URL routing) wins when present; this middleware
// fills the gap when none of those fired.
//
// Empty stamps list returns `next` unchanged.
//
// Per-key existence check is via `metadata.FromOutgoingContext`
// — reads the outgoing-context-stored MD without copying. When
// the key already has at least one value, this middleware is a
// no-op for that key.
func MetadataDefaultStampMiddleware(stamps []DefaultMetadataStamp, next http.Handler) http.Handler {
	if len(stamps) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		md, _ := metadata.FromOutgoingContext(ctx)
		var pairs []string
		for _, s := range stamps {
			if len(md.Get(s.Key)) == 0 {
				pairs = append(pairs, s.Key, s.Value)
			}
		}
		if len(pairs) > 0 {
			ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}
