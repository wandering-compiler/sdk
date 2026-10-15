package imagevariant

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func size(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	c, kind, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return c.Width, c.Height, kind
}

func TestRender_FitsInsideTheBoxKeepingTheAspect(t *testing.T) {
	out, ct, err := Render(bytes.NewReader(pngOf(t, 1200, 600)), Spec{Name: "card", MaxWidth: 400, MaxHeight: 400})
	if err != nil {
		t.Fatal(err)
	}
	if w, h, kind := size(t, out); w != 400 || h != 200 || kind != "png" || ct != "image/png" {
		t.Errorf("got %dx%d %s (%s), want 400x200 png", w, h, kind, ct)
	}
	// Only one side bounded.
	out, _, err = Render(bytes.NewReader(pngOf(t, 600, 1200)), Spec{Name: "tall", MaxHeight: 300})
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := size(t, out); w != 150 || h != 300 {
		t.Errorf("height-bound: %dx%d, want 150x300", w, h)
	}
}

func TestRender_NeverEnlarges(t *testing.T) {
	out, _, err := Render(bytes.NewReader(pngOf(t, 100, 50)), Spec{Name: "big", MaxWidth: 800, MaxHeight: 800})
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := size(t, out); w != 100 || h != 50 {
		t.Errorf("a small source was enlarged to %dx%d", w, h)
	}
}

func TestRender_FormatFollowsTheSourceOrTheSpec(t *testing.T) {
	out, ct, err := Render(bytes.NewReader(jpegOf(t, 800, 800)), Spec{Name: "t", MaxWidth: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, kind := size(t, out); kind != "jpeg" || ct != "image/jpeg" {
		t.Errorf("a JPEG source's auto variant is %s (%s)", kind, ct)
	}
	out, ct, err = Render(bytes.NewReader(pngOf(t, 800, 800)), Spec{Name: "t", MaxWidth: 100, Format: JPEG, Quality: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, kind := size(t, out); kind != "jpeg" || ct != "image/jpeg" {
		t.Errorf("format: jpeg gave %s (%s)", kind, ct)
	}
}

func TestRender_Refusals(t *testing.T) {
	if _, _, err := Render(bytes.NewReader([]byte("<svg onload=alert(1)>")), Spec{Name: "t", MaxWidth: 10}); !errors.Is(err, ErrNotAnImage) {
		t.Errorf("a non-image: %v", err)
	}
	// A header declaring 20000x20000 (400 MP) in a few bytes: refused from
	// the header, before a pixel is allocated.
	var bomb bytes.Buffer
	_ = png.Encode(&bomb, image.NewGray(image.Rect(0, 0, 1, 1)))
	b := bomb.Bytes()
	// IHDR width/height live at bytes 16..23 of a PNG.
	copy(b[16:24], []byte{0, 0, 0x4e, 0x20, 0, 0, 0x4e, 0x20})
	// …and the chunk's CRC (over type + data, bytes 12..28), or the decoder
	// refuses the file as corrupt before it ever reads the size.
	binary.BigEndian.PutUint32(b[29:33], crc32.ChecksumIEEE(b[12:29]))
	if _, _, err := Render(bytes.NewReader(b), Spec{Name: "t", MaxWidth: 10}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a 400 MP header: %v", err)
	}
	if _, _, err := Render(bytes.NewReader(pngOf(t, 10, 10)), Spec{Name: "none"}); err == nil {
		t.Error("a variant bounding neither side was accepted")
	}
}
