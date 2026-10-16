package restgw

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
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

	// Variants are the field's declared image variants
	// (`(w17.field).upload.variants`): `?variant=<name>` serves one,
	// rendered on first request (or by the variant workers right after the
	// upload) and stored beside the original. Empty = originals only.
	Variants []imagevariant.Spec

	// Image marks an UPLOADED_IMAGE field: its originals are served with the
	// raster type their bytes are (see imageType) instead of
	// application/octet-stream, so a browser renders them inline. Anything
	// that is not one of those types still goes out as octet-stream.
	Image bool
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
		writeRefusalStatus(r.Context(), w, http.StatusMethodNotAllowed, codes.Unimplemented, "")
		return
	}
	// Every refusal from this handler is the standard JSON error
	// envelope, the same shape a client parses from any other route.
	notFound := func() { writeRefusalStatus(r.Context(), w, http.StatusNotFound, codes.NotFound, "") }
	prefix := cfg.URLPrefix + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		notFound()
		return
	}
	remainder := r.URL.Path[len(prefix):]
	if remainder == "" {
		notFound()
		return
	}
	// Reject path traversal before it reaches the driver. chi does not
	// clean `..` segments by default, and the storage drivers resolve
	// keys via filepath.Join (which collapses `..`), so an un-sanitized
	// remainder like `avatars/../../../etc/passwd` would otherwise escape
	// the bucket root. 404 (not 403) keeps the existing no-leak posture.
	if hasDotSegment(remainder) {
		notFound()
		return
	}

	field, key, ok := matchBucket(cfg.Fields, remainder)
	if !ok {
		notFound()
		return
	}

	ctx := r.Context()
	if name, asked := r.URL.Query()["variant"]; asked {
		serveVariant(w, r, cfg, field, key, strings.Join(name, ","))
		return
	}
	info, err := field.Driver.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			notFound()
			return
		}
		// Don't reflect the driver error (can carry FS paths / backing-
		// store internals) — the operator gets it, the client a generic 500.
		WriteInternal(ctx, w, "download: stat: "+err.Error())
		return
	}
	body, err := field.Driver.OpenSeekable(ctx, key)
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			notFound()
			return
		}
		WriteInternal(ctx, w, "download: open: "+err.Error())
		return
	}
	defer func() { _ = body.Close() }()

	objectKey := key[strings.LastIndex(key, "/")+1:]

	// An image field's original carries the type its bytes are, when they
	// are a raster image a browser renders and never executes; every other
	// upload — and an image field's upload that is not one — is
	// application/octet-stream (below).
	contentType, ext := "application/octet-stream", ""
	if field.Image {
		var err error
		if contentType, ext, err = sniffImageType(body); err != nil {
			WriteInternal(ctx, w, "download: sniff: "+err.Error())
			return
		}
	}

	header := w.Header()
	if !cfg.DisableETag {
		header.Set("ETag", `"`+objectKey+`"`)
	}
	if cfg.CacheControl != "" {
		header.Set("Cache-Control", cfg.CacheControl)
	}
	if cfg.AsAttachment {
		header.Set("Content-Disposition", `attachment; filename="`+objectKey+ext+`"`)
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

	// These are user-UPLOADED bytes served same-origin,
	// and the upload allowlist validates only the filename extension,
	// never the content. Without an explicit Content-Type,
	// http.ServeContent sniffs the body (the empty `name` disables the
	// extension path), so an uploaded HTML / SVG / JS payload would be
	// served as `text/html` and, inline on the same origin, EXECUTED —
	// stored XSS. So the type is always set here, never left to sniffing:
	// application/octet-stream, or — on an image field — one of the raster
	// types sniffImageType admits, which a browser renders and never runs.
	// `nosniff` keeps the browser to the type it is given.
	// Setting Content-Type before ServeContent also stops it sniffing.
	header.Set("Content-Type", contentType)
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
	// One slash, whatever the two sides bring. `kvfs.BuildKey` returns a
	// key that STARTS with the bucket path's leading `/`, so a plain
	// `prefix + "/" + key` yielded `…/files//task-attachments/…` — and
	// ServeDownload, which strips `prefix + "/"` and hands the rest to
	// matchBucket (which looks for a bucket WITHOUT a leading slash),
	// answered 404 to every URL this function produced. The write path
	// and the read path disagreed about one character, and both were
	// green because nothing ever fed one's output to the other.
	//
	// The empty path segment was not merely cosmetic either: a proxy
	// that merges slashes (nginx does, by default) rewrites the request
	// before the gateway sees it, so the same URL means different things
	// depending on what sits in front.
	path := prefix + "/" + strings.TrimPrefix(storageKey, "/")
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
// normalised to '/' before the segment scan.
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

// serveVariant serves one declared image variant of the object at key.
//
// The variant is rendered the first time it is asked for and stored in the
// same store under `<key>~<name>`, so every later request — from this replica
// or another — reads it back instead of resizing again. Concurrent first
// requests may each render it; the store's atomic put keeps one whole copy.
//
// Only declared names exist (400 for any other): a server that resized to
// whatever a URL asked for would render and store a new image per request.
// The bytes are this server's own re-encoding, never the upload's, so they are
// served as the image type they are rather than as an opaque download.
func serveVariant(w http.ResponseWriter, r *http.Request, cfg DownloadConfig, field DownloadField, key, name string) {
	ctx := r.Context()
	var spec imagevariant.Spec
	ok := false
	for _, v := range field.Variants {
		if v.Name == name {
			spec, ok = v, true
		}
	}
	if !ok {
		writeRefusalStatus(ctx, w, http.StatusBadRequest, codes.InvalidArgument, "")
		return
	}
	vkey := VariantKey(key, name)
	info, err := field.Driver.Stat(ctx, vkey)
	if errors.Is(err, kvfs.ErrNotFound) {
		if status, rerr := renderVariant(ctx, field.Driver, key, vkey, spec); rerr != nil {
			if status == http.StatusInternalServerError {
				WriteInternal(ctx, w, "download variant: "+rerr.Error())
			} else {
				writeRefusalStatus(ctx, w, status, codes.InvalidArgument, "")
			}
			return
		}
		info, err = field.Driver.Stat(ctx, vkey)
	}
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			writeRefusalStatus(ctx, w, http.StatusNotFound, codes.NotFound, "")
			return
		}
		WriteInternal(ctx, w, "download variant: stat: "+err.Error())
		return
	}
	body, err := field.Driver.OpenSeekable(ctx, vkey)
	if err != nil {
		WriteInternal(ctx, w, "download variant: open: "+err.Error())
		return
	}
	defer func() { _ = body.Close() }()
	contentType, ext, err := sniffImageType(body)
	if err != nil {
		WriteInternal(ctx, w, "download variant: sniff: "+err.Error())
		return
	}
	objectKey := vkey[strings.LastIndex(vkey, "/")+1:]
	header := w.Header()
	if !cfg.DisableETag {
		header.Set("ETag", `"`+objectKey+`"`)
	}
	if cfg.CacheControl != "" {
		header.Set("Cache-Control", cfg.CacheControl)
	}
	if cfg.AsAttachment {
		header.Set("Content-Disposition", `attachment; filename="`+objectKey+ext+`"`)
	} else {
		header.Set("Content-Disposition", "inline")
	}
	if cfg.DisableRange {
		r.Header.Del("Range")
		r.Header.Del("If-Range")
	}
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime, body)
}

// VariantKey is where a variant of the object at key is stored: beside it,
// under a name no upload produces (object keys are hex digests).
func VariantKey(key, name string) string { return key + "~" + name }

// renderVariant renders and stores one variant. The status is the answer a
// failure deserves: 404 for a missing original, 415 for one that is not an
// image this server resizes, 500 for the store.
func renderVariant(ctx context.Context, d kvfs.Driver, key, vkey string, spec imagevariant.Spec) (int, error) {
	src, err := d.Open(ctx, key)
	if err != nil {
		if errors.Is(err, kvfs.ErrNotFound) {
			return http.StatusNotFound, err
		}
		return http.StatusInternalServerError, err
	}
	data, _, err := imagevariant.Render(src, spec)
	_ = src.Close()
	if errors.Is(err, imagevariant.ErrNotAnImage) || errors.Is(err, imagevariant.ErrTooLarge) {
		return http.StatusUnsupportedMediaType, err
	}
	if err != nil {
		return http.StatusInternalServerError, err
	}
	dir := ""
	if sp, ok := d.(kvfs.StagingDirProvider); ok {
		if dir, err = sp.StagingDir(); err != nil {
			return http.StatusInternalServerError, err
		}
	}
	tmp, err := os.CreateTemp(dir, "w17-variant-*")
	if err != nil {
		return http.StatusInternalServerError, err
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpPath)
		return http.StatusInternalServerError, errors.Join(werr, cerr)
	}
	if _, err := d.PutFromTempFile(ctx, vkey, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return http.StatusInternalServerError, err
	}
	return 0, nil
}

// imageTypes are the content types an image field's bytes may be served as:
// raster formats a browser decodes and never executes, with the extension an
// attachment's filename gets. SVG is deliberately absent — it is a document
// that runs script — and http.DetectContentType reports it as XML anyway.
var imageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// sniffImageType reads the head of body and rewinds it: the raster type the
// bytes are, with its extension, or application/octet-stream and no extension
// when they are anything else. The decision is the content's, never the
// uploaded filename's — the upload checked only that.
func sniffImageType(body io.ReadSeeker) (contentType, ext string, err error) {
	var head [512]byte
	n, err := io.ReadFull(body, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	ct := http.DetectContentType(head[:n])
	if ext, ok := imageTypes[ct]; ok {
		return ct, ext, nil
	}
	return "application/octet-stream", "", nil
}
