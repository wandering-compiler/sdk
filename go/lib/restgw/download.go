package restgw

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// DownloadConfig drives one Slice C download endpoint. The
// gateway generator emits one DownloadConfig per
// DownloadEndpoint declared on the surface and hands it to
// [ServeDownload] from inside the per-endpoint handler closure.
//
// Fields[].BucketPath is the URL prefix segment between
// `<URLPrefix>/` and the per-object key — uniqueness across
// the slice is parser-enforced (Slice C2).
type DownloadConfig struct {
	// URLPrefix is the composed `<api.prefix><group.prefix><download.path>`
	// — the same value Method.HTTPPath uses for unary endpoints.
	// No trailing slash; the handler strips
	// `URLPrefix + "/"` from r.URL.Path before dispatch.
	URLPrefix string

	// Fields is the resolved set of UPLOADED_* fields the
	// endpoint serves. Each entry pairs a BucketPath prefix
	// with the kvfs.Driver that actually reads the bytes. The
	// constructor (NewDownloadConfig) sorts the slice by
	// descending BucketPath length so longest-prefix match is
	// a single forward scan at request time.
	Fields []DownloadField

	// CacheControl is the verbatim Cache-Control header value.
	// Empty = no header emitted.
	CacheControl string

	// AsAttachment switches Content-Disposition from `inline`
	// to `attachment; filename="<object_key>"`. False (default)
	// = inline.
	AsAttachment bool

	// DisableETag suppresses the ETag header. The default
	// behaviour emits `ETag: "<object_key>"` because object
	// keys are sha256 hex (Slice A) — strong-validator for free.
	DisableETag bool

	// DisableRange suppresses Range-request handling — the
	// handler clears r.Header["Range"] before delegating to
	// http.ServeContent so partial responses never go out.
	DisableRange bool
}

// DownloadField is one resolved UPLOADED_* field served by a
// download endpoint. The BucketPath is the URL prefix segment
// between the endpoint's URLPrefix and the object key.
type DownloadField struct {
	// BucketPath is the URL prefix segment for this field's
	// bucket. Always starts with `/`, no trailing slash.
	BucketPath string

	// Driver is the kvfs.Driver backing this bucket. Resolved
	// at gateway boot from the field's `(w17.field).upload.connection`.
	Driver kvfs.Driver
}

// NewDownloadConfig sorts the field list by descending bucket-
// path length and returns the result. Use when assembling a
// DownloadConfig in generated code so the handler doesn't pay
// the sort cost per request.
func NewDownloadConfig(prefix string, fields []DownloadField, cacheControl string, asAttachment, disableETag, disableRange bool) DownloadConfig {
	cp := append([]DownloadField(nil), fields...)
	sort.Slice(cp, func(i, j int) bool {
		return len(cp[i].BucketPath) > len(cp[j].BucketPath)
	})
	return DownloadConfig{
		URLPrefix:    prefix,
		Fields:       cp,
		CacheControl: cacheControl,
		AsAttachment: asAttachment,
		DisableETag:  disableETag,
		DisableRange: disableRange,
	}
}

// ServeDownload is the post-auth handler body — the generator
// emits a per-endpoint closure that runs auth + scope checks
// and then calls into here.
//
// Path resolution:
//   - Strip cfg.URLPrefix + "/" from r.URL.Path.
//   - Longest-prefix match against cfg.Fields[].BucketPath
//     (with leading "/" trimmed since the URL already
//     contributes the slash). Miss = 404.
//   - Reconstruct storage key as `<bucket_path>/<remainder>` —
//     the same shape Slice A's BuildKey uses, so
//     driver.Stat / driver.OpenSeekable hit the same key the
//     upload handler wrote.
//   - 404 (not 403) when no field matches: leaking which
//     buckets exist gives an attacker a directory listing for
//     free.
func ServeDownload(w http.ResponseWriter, r *http.Request, cfg DownloadConfig) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := cfg.URLPrefix + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	remainder := r.URL.Path[len(prefix):]
	if remainder == "" {
		http.NotFound(w, r)
		return
	}
	// Reject path traversal before it reaches the driver. chi does not
	// clean `..` segments by default, and the storage drivers resolve
	// keys via filepath.Join (which collapses `..`), so an un-sanitized
	// remainder like `avatars/../../../etc/passwd` would otherwise escape
	// the bucket root. 404 (not 403) keeps the existing no-leak posture.
	if hasDotSegment(remainder) {
		http.NotFound(w, r)
		return
	}

	field, key, ok := matchBucket(cfg.Fields, remainder)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	info, err := field.Driver.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		// Don't reflect the driver error (can carry FS paths / backing-
		// store internals) — log it and return a generic 500 (restgw-sec-3).
		observx.ReportError(ctx, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := field.Driver.OpenSeekable(ctx, key)
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		observx.ReportError(ctx, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()

	objectKey := key[strings.LastIndex(key, "/")+1:]

	header := w.Header()
	if !cfg.DisableETag {
		header.Set("ETag", `"`+objectKey+`"`)
	}
	if cfg.CacheControl != "" {
		header.Set("Cache-Control", cfg.CacheControl)
	}
	if cfg.AsAttachment {
		header.Set("Content-Disposition", `attachment; filename="`+objectKey+`"`)
	} else {
		header.Set("Content-Disposition", "inline")
	}
	if cfg.DisableRange {
		// http.ServeContent honours Range when the request
		// carries the header; clearing the slot disables the
		// entire byte-range subsystem (no 206, no
		// If-Range comparison). We do this by mutating the
		// request's header map, which is safe — http.ServeMux
		// hands each handler a request struct it owns for the
		// duration of the call.
		r.Header.Del("Range")
		r.Header.Del("If-Range")
	}

	// Q36-restgw-2 — these are user-UPLOADED bytes served same-origin,
	// and the upload allowlist validates only the filename extension,
	// never the content. Without an explicit Content-Type,
	// http.ServeContent sniffs the body (the empty `name` disables the
	// extension path), so an uploaded HTML / SVG / JS payload would be
	// served as `text/html` and, inline on the same origin, EXECUTED —
	// stored XSS. Pin a non-renderable type + `nosniff` so the browser
	// never sniffs or executes uploaded content. (Per-field declared
	// content types for safe inline rendering of images / PDFs are a
	// future enhancement; until then security wins over inline preview.)
	// Setting Content-Type before ServeContent also stops it sniffing.
	header.Set("Content-Type", "application/octet-stream")
	header.Set("X-Content-Type-Options", "nosniff")

	// http.ServeContent handles If-None-Match (against ETag) +
	// If-Modified-Since (against ModTime) automatically.
	http.ServeContent(w, r, "", info.ModTime, body)
}

// BuildDownloadURL composes the externally-visible URL for a
// stored object given the endpoint's URL prefix, the runtime
// public host, and the kvfs storage key.
//
//   - `urlForm` selects RELATIVE (path-only) or ABSOLUTE
//     (host-prefixed). The string matches what the parser
//     emits in `parser.Download.URLForm`.
//   - `prefix` is the endpoint's URLPrefix (`<api.prefix>
//     <group.prefix><download.path>`), no trailing slash.
//   - `host` is the runtime base URL (e.g. `https://files.example.com`)
//     read from `<PREFIX>_PUBLIC_HOST`. Required when urlForm
//     == "ABSOLUTE"; ignored when "RELATIVE". Trailing slash
//     is trimmed defensively.
//   - `storageKey` is the kvfs key the upload handler stored
//     (`<bucket>/<sub>/<object_key>`); used as-is — no
//     normalisation since the upload + download paths agree
//     on the shape.
//
// Empty `storageKey` returns "" — the gateway uses this as
// the "no file uploaded yet" sentinel and the FE renders a
// placeholder. Empty `prefix` is allowed (api may sit at the
// root). Empty `host` with urlForm="ABSOLUTE" is an
// operator misconfiguration that the main.go fail-fast
// boot check should catch; here we still emit the
// path-only URL so a request doesn't outright crash.
func BuildDownloadURL(urlForm, prefix, host, storageKey string) string {
	if storageKey == "" {
		return ""
	}
	path := prefix + "/" + storageKey
	if urlForm == "ABSOLUTE" && host != "" {
		return strings.TrimRight(host, "/") + path
	}
	return path
}

// hasDotSegment reports whether any path segment of p is "." or
// ".." — the building blocks of a path-traversal attempt.
//
// Both separators are considered: a storage driver resolving keys
// via filepath.Join collapses ".." regardless of which slash the
// request used, and on a Windows host '\' is also a path
// separator. Splitting on '/' alone would let
// `avatars\..\..\etc\passwd` slip past the guard, so '\' is
// normalised to '/' before the segment scan. (R-restgw-6)
func hasDotSegment(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// matchBucket returns the field whose BucketPath is the longest
// prefix of `remainder` (after the leading `/` of BucketPath is
// trimmed — the URL already contributes that slash). Returns
// the resolved field, the full storage key, and ok=false when
// no field matches.
//
// `cfg.Fields` is expected to be sorted by descending
// BucketPath length (NewDownloadConfig does this); the first
// match is therefore the longest. Linear scan is fine for the
// expected N (≤ tens of fields per endpoint in practice).
func matchBucket(fields []DownloadField, remainder string) (DownloadField, string, bool) {
	for _, f := range fields {
		// Strip leading `/` from BucketPath: URL contributes
		// it. Empty BucketPath shouldn't reach here (parser
		// rejects), but guard anyway.
		bp := strings.TrimPrefix(f.BucketPath, "/")
		if bp == "" {
			continue
		}
		if !strings.HasPrefix(remainder, bp+"/") {
			continue
		}
		// Storage key as Slice A wrote it: `<bucket_path>/<rest>`
		// where bucket_path begins with `/`. Reconstruct.
		key := f.BucketPath + "/" + remainder[len(bp)+1:]
		// Trim the leading `/` because the kvfs callers
		// store keys without it.
		key = strings.TrimPrefix(key, "/")
		return f, key, true
	}
	return DownloadField{}, "", false
}
