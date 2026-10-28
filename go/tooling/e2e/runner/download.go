package runner

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/url"
	"strings"

	_ "golang.org/x/image/webp" // registers the WebP decoder for imageSize

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
)

// Download makes a step FETCH a stored file through one of the surface's
// download routes, in place of calling a method. It is how a scenario
// asserts what a download route serves — the bytes' type and size, an image
// variant's dimensions — and to whom: a route behind auth refuses a step that
// is `anonymous`, one with `exclude_auth` serves it.
//
// The response is reported through WireKey, as a `raw_response` answer is,
// plus `width` / `height` when the body is an image; a refusal is the usual
// error envelope, asserted with `expect_error`.
type Download struct {
	// URL is the file's URL as a response rendered it for an UPLOADED_*
	// field, interpolated (`${cover.image_path}`). Only its PATH from the
	// route prefix on is used: a URL_FORM_ABSOLUTE value carries the public
	// host the deployment was configured with — possibly with a path, behind
	// a sub-path proxy — which is not where this run's gateway listens. A URL
	// with a query is refused: no rendered URL carries one today, and one
	// that did (a signature) would be dropped, so a refusal could pass for
	// the wrong reason.
	URL string

	// Route, when set, is the composed prefix of the download route to ask
	// for the same object — the URL's own route prefix is replaced with it.
	// It is how a step proves that a route does NOT serve a field (a 404),
	// or does not serve it to everyone (a 401), when the response renders
	// that field under a different route. Empty: the URL's own route.
	Route string

	// Variant is the `?variant=` to ask for. Empty: the original.
	Variant string

	// Routes are every download route of the surface. The runner finds the
	// one the URL is under by longest prefix, which is also how it knows
	// whether the request carries the session token.
	Routes []DownloadRoute
}

// DownloadRoute is one download route of a surface: its composed URL prefix
// and whether it runs behind the auth middleware.
type DownloadRoute struct {
	Prefix       string
	AuthRequired bool
}

// ImageFile asks the runner to GENERATE the bytes of a file part: an image
// of these dimensions, encoded in Format ("png", "jpeg" or "gif"). An image
// cannot be written as inline text, and a fixture of opaque base64 is one
// nobody can read — this one says what the file is.
type ImageFile struct {
	Format        string
	Width, Height int
}

// resolveDownload turns a download step into the call the REST caller makes:
// a GET of the object's path under the route it is asked from, the
// `variant` as its query, the route's auth flag deciding the token.
func resolveDownload(d Download, scope *runtime.Scope) (Endpoint, map[string]any, error) {
	raw, err := expandOne(d.URL, scope)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("download: expand url: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return Endpoint{}, nil, fmt.Errorf("download: the url %q is empty — the response that should have rendered it did not", d.URL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("download: url %q: %w", raw, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, nil, fmt.Errorf("download: url %q carries a query or fragment — the step sends only the path and `variant`, so it would not be what the response rendered", raw)
	}
	path := u.EscapedPath()
	own, at, ok := routeOf(path, d.Routes)
	if !ok {
		return Endpoint{}, nil, fmt.Errorf("download: %q is under no download route of this surface (%s) — a field no route covers comes back as the raw stored handle, not a URL", raw, routePrefixes(d.Routes))
	}
	path = path[at:]
	route := own
	if d.Route != "" {
		found := false
		for _, r := range d.Routes {
			if r.Prefix == d.Route {
				route, found = r, true
				break
			}
		}
		if !found {
			return Endpoint{}, nil, fmt.Errorf("download: route %q is not a download route of this surface (%s)", d.Route, routePrefixes(d.Routes))
		}
		path = route.Prefix + path[len(own.Prefix):]
	}
	ep := Endpoint{
		Ref:          "download " + route.Prefix,
		Transport:    "rest",
		AuthRequired: route.AuthRequired,
		HTTPMethod:   "GET",
		PathTemplate: path,
		RawResponse:  true,
	}
	input := map[string]any{}
	if d.Variant != "" {
		input["variant"] = d.Variant
	}
	return ep, input, nil
}

// routeOf is the route a path is under and the offset its prefix starts at:
// the longest prefix the path continues past a `/`, the same rule the
// gateway's own mux applies. A path the public host prefixed (`/app` behind
// a sub-path proxy) matches where the earliest route prefix starts.
func routeOf(path string, routes []DownloadRoute) (DownloadRoute, int, bool) {
	var best DownloadRoute
	at, found := 0, false
	for _, r := range routes {
		i := strings.Index(path, r.Prefix+"/")
		if i < 0 || (found && i > at) || (found && i == at && len(r.Prefix) < len(best.Prefix)) {
			continue
		}
		best, at, found = r, i, true
	}
	return best, at, found
}

func routePrefixes(routes []DownloadRoute) string {
	if len(routes) == 0 {
		return "it declares none"
	}
	ps := make([]string, len(routes))
	for i, r := range routes {
		ps[i] = r.Prefix
	}
	return "routes: " + strings.Join(ps, ", ")
}

// imageSize reads the dimensions of an image body, false when the bytes are
// not an image this runner decodes (PNG, JPEG, GIF, WebP).
func imageSize(raw []byte) (int, int, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// maxImageSide bounds a generated image, as the spec does: a size off by a
// few zeros is an error here, not gigabytes of pixels.
const maxImageSide = 4096

// renderImage encodes a generated image of the declared size. The pixels are
// a gradient rather than one flat colour, so a resize has something to
// resample and the encoded size depends on the dimensions.
func renderImage(f ImageFile) ([]byte, error) {
	if f.Width <= 0 || f.Height <= 0 || f.Width > maxImageSide || f.Height > maxImageSide {
		return nil, fmt.Errorf("image %dx%d: width and height must be between 1 and %d", f.Width, f.Height, maxImageSide)
	}
	img := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / f.Width), G: uint8(y * 255 / f.Height), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	var err error
	switch f.Format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		return nil, fmt.Errorf("image format %q: the runner generates png, jpeg or gif", f.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", f.Format, err)
	}
	return buf.Bytes(), nil
}
