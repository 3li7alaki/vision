package diff

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"vision/internal/store"
)

func imagePNG(t *testing.T, colors ...color.RGBA) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, len(colors), 1))
	for x, c := range colors {
		im.SetRGBA(x, 0, c)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDigestAndPNG(t *testing.T) {
	a := imagePNG(t, color.RGBA{R: 1, A: 255}, color.RGBA{G: 1, A: 255})
	b := imagePNG(t, color.RGBA{R: 1, A: 255}, color.RGBA{B: 1, A: 255})
	if Digest(a) == Digest(b) {
		t.Fatal("different images had same digest")
	}
	got, fraction, err := PNG(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || fraction != .5 {
		t.Fatalf("got %d bytes and fraction %v", len(got), fraction)
	}
}

func TestComparable(t *testing.T) {
	a := store.Conditions{Width: 375, Height: 812, DPR: 2, URL: "http://x", Scheme: "dark"}
	if err := Comparable(a, a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Width = 390
	if err := Comparable(a, b); err == nil {
		t.Fatal("mismatched conditions accepted")
	}
}

// A scheme or URL mismatch used to print two identical geometries, which reads as a bug
// in vision rather than a light shot held against a dark baseline.
func TestComparableNamesTheFieldThatDiffers(t *testing.T) {
	a := store.Conditions{Width: 375, Height: 812, DPR: 2, URL: "http://x", Scheme: "dark"}
	for _, tc := range []struct {
		name   string
		change func(*store.Conditions)
		want   string
	}{
		{"scheme", func(c *store.Conditions) { c.Scheme = "light" }, "scheme"},
		{"empty scheme", func(c *store.Conditions) { c.Scheme = "" }, "scheme"},
		{"url", func(c *store.Conditions) { c.URL = "http://y" }, "url"},
		{"size", func(c *store.Conditions) { c.DPR = 1 }, "size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := a
			tc.change(&b)
			err := Comparable(a, b)
			if err == nil {
				t.Fatal("mismatched conditions accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
