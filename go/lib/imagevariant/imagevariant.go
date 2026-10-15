// Package imagevariant renders the declared size variants of an uploaded
// image: a thumbnail, a card, a preview — each fitted inside a box with its
// aspect ratio kept, never enlarged, and re-encoded as JPEG or PNG.
//
// Only declared variants exist. A server that resized to any size a URL asked
// for would render — and store — a new image per request, so the variants are
// a closed set named in the REST registry and the URL picks one by name.
//
// A source is measured before it is decoded: an image's header states its
// size, and a small file can declare a huge one (a decompression bomb), so a
// source over MaxSourcePixels is refused without allocating it.
package imagevariant

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	_ "image/gif" // a GIF source decodes (its first frame)

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // a WebP source decodes; output is JPEG or PNG
)

// Format is a variant's output encoding.
type Format string

const (
	// Auto keeps the source's family: JPEG for a JPEG source, PNG for every
	// other one (PNG, GIF, WebP — which may carry transparency).
	Auto Format = ""
	JPEG Format = "jpeg"
	PNG  Format = "png"
)

// MaxSourcePixels bounds what a source may declare (width × height) before
// it is decoded: 50 megapixels, above any camera photograph.
const MaxSourcePixels = 50_000_000

// DefaultQuality is the JPEG quality when a variant names none.
const DefaultQuality = 85

// Spec is one declared variant.
type Spec struct {
	// Name is how a URL asks for it (?variant=<name>).
	Name string
	// MaxWidth / MaxHeight bound the output; 0 leaves that side unbounded.
	// At least one is set.
	MaxWidth, MaxHeight int
	Format              Format
	// Quality is the JPEG quality (1–100); 0 = DefaultQuality.
	Quality int
}

// ErrNotAnImage is a source no registered decoder reads.
var ErrNotAnImage = errors.New("imagevariant: the stored file is not an image this server decodes (JPEG, PNG, GIF, WebP)")

// ErrTooLarge is a source over MaxSourcePixels.
var ErrTooLarge = errors.New("imagevariant: the stored image is larger than this server resizes")

// Render reads src and returns the variant's bytes and its content type.
func Render(src io.Reader, s Spec) ([]byte, string, error) {
	if s.MaxWidth < 0 || s.MaxHeight < 0 || (s.MaxWidth == 0 && s.MaxHeight == 0) {
		return nil, "", fmt.Errorf("imagevariant: variant %q bounds neither side", s.Name)
	}
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, "", fmt.Errorf("imagevariant: read: %w", err)
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrNotAnImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return nil, "", ErrTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", ErrNotAnImage
	}
	w, h := fit(img.Bounds().Dx(), img.Bounds().Dy(), s.MaxWidth, s.MaxHeight)
	out := img
	if w != img.Bounds().Dx() || h != img.Bounds().Dy() {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		out = dst
	}
	format := s.Format
	if format == Auto {
		format = PNG
		if kind == "jpeg" {
			format = JPEG
		}
	}
	var buf bytes.Buffer
	switch format {
	case JPEG:
		q := s.Quality
		if q <= 0 || q > 100 {
			q = DefaultQuality
		}
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: q}); err != nil {
			return nil, "", fmt.Errorf("imagevariant: encode jpeg: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	case PNG:
		if err := png.Encode(&buf, out); err != nil {
			return nil, "", fmt.Errorf("imagevariant: encode png: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
	return nil, "", fmt.Errorf("imagevariant: variant %q: format %q is not jpeg or png", s.Name, s.Format)
}

// Ext is the file extension of a variant's output for a source of kind
// (the image package's format name).
func (s Spec) Ext(kind string) string {
	switch s.Format {
	case JPEG:
		return ".jpg"
	case PNG:
		return ".png"
	}
	if kind == "jpeg" {
		return ".jpg"
	}
	return ".png"
}

// fit scales w×h down into the box, keeping the aspect ratio; it never
// enlarges, and never rounds a side to zero.
func fit(w, h, maxW, maxH int) (int, int) {
	scale := 1.0
	if maxW > 0 && w > maxW {
		scale = float64(maxW) / float64(w)
	}
	if maxH > 0 && float64(h)*scale > float64(maxH) {
		scale = float64(maxH) / float64(h)
	}
	if scale >= 1 {
		return w, h
	}
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	return max(nw, 1), max(nh, 1)
}
