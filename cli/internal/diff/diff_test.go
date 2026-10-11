package diff

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"
	"vision/internal/store"
)

func imagePNG(t *testing.T, colors ...color.RGBA) []byte {
	t.Helper()
	return sizedPNG(t, len(colors), 1, colors...)
}

func sizedPNG(t *testing.T, width, height int, colors ...color.RGBA) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, width, height))
	for i, c := range colors {
		im.SetRGBA(i%width, i/width, c)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPNGPadsDifferentSizes(t *testing.T) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	oldPixel, newPixel := color.RGBA{R: 1, A: 255}, color.RGBA{B: 1, A: 255}
	gray := color.RGBA{R: 127, G: 127, B: 127, A: 255}
	red := color.RGBA{R: 244, G: 63, B: 94, A: 255}
	amber := color.RGBA{R: 251, G: 191, B: 36, A: 255}
	for _, tc := range []struct {
		name     string
		base     []byte
		shot     []byte
		fraction float64
		pixels   []color.RGBA
	}{
		{"height grows", sizedPNG(t, 2, 1, white, white), sizedPNG(t, 2, 2, white, white), .5, []color.RGBA{gray, gray, amber, amber}},
		{"height shrinks and pixel changes", sizedPNG(t, 2, 2, white, oldPixel), sizedPNG(t, 2, 1, white, newPixel), .75, []color.RGBA{gray, red, amber, amber}},
		{"width grows", sizedPNG(t, 1, 2, white, oldPixel), sizedPNG(t, 2, 2, white, color.RGBA{}, newPixel), .75, []color.RGBA{gray, amber, red, amber}},
		{"opposite axes", sizedPNG(t, 2, 1, oldPixel), sizedPNG(t, 1, 2, newPixel), .75, []color.RGBA{red, amber, amber, {A: 255}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, fraction, err := PNG(tc.base, tc.shot)
			if err != nil {
				t.Fatal(err)
			}
			if fraction != tc.fraction {
				t.Fatalf("fraction: got %v, want %v", fraction, tc.fraction)
			}
			im, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if im.Bounds() != image.Rect(0, 0, 2, 2) {
				t.Fatalf("bounds: got %v, want 2x2", im.Bounds())
			}
			for i, want := range tc.pixels {
				if got := color.RGBAModel.Convert(im.At(i%2, i/2)).(color.RGBA); got != want {
					t.Errorf("pixel (%d,%d): got %v, want %v", i%2, i/2, got, want)
				}
			}
		})
	}
}

func TestChanges(t *testing.T) {
	base := store.Conditions{Width: 1440, Height: 700, DPR: 2, URL: "http://x", Scheme: "dark"}
	shot := base
	shot.Height = 757
	shot.URL = "http://y"
	old, cur := sizedPNG(t, 2, 1), sizedPNG(t, 2, 2)
	for _, tc := range []struct {
		name string
		shot store.Conditions
		png  []byte
		want []string
	}{
		{"size and url", shot, cur, []string{"size 2x1 to 2x2", "url http://x to http://y"}},
		{"size only", base, cur, []string{"size 2x1 to 2x2"}},
		{"url only", shot, old, []string{"url http://x to http://y"}},
		{"identical", base, old, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Changes(base, tc.shot, old, tc.png); !slices.Equal(got, tc.want) {
				t.Fatalf("changes: got %v, want %v", got, tc.want)
			}
		})
	}
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

// Refusals must name the changed condition so identical geometries cannot hide a scheme mismatch.
func TestComparableNamesTheFieldThatDiffers(t *testing.T) {
	a := store.Conditions{Width: 375, Height: 812, DPR: 2, URL: "http://x", Scheme: "dark"}
	for _, tc := range []struct {
		name   string
		change func(*store.Conditions)
		want   string
	}{
		{"scheme", func(c *store.Conditions) { c.Scheme = "light" }, `scheme "dark" vs "light"`},
		{"empty scheme", func(c *store.Conditions) { c.Scheme = "" }, `scheme "dark" vs ""`},
		{"url accepted", func(c *store.Conditions) { c.URL = "http://y" }, ""},
		{"height accepted", func(c *store.Conditions) { c.Height = 757 }, ""},
		{"width", func(c *store.Conditions) { c.Width = 390 }, "viewport width 375 vs 390"},
		{"dpr", func(c *store.Conditions) { c.DPR = 1 }, "pixel ratio 2 vs 1"},
		{"selector", func(c *store.Conditions) { c.Selector = "#cart" }, `whole viewport vs element "#cart"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := a
			tc.change(&b)
			err := Comparable(a, b)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("comparable conditions refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("mismatched conditions accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestComparableRefusesUnknownSchemes(t *testing.T) {
	for _, schemes := range [][2]string{{"", "dark"}, {"dark", ""}, {"", ""}} {
		a := store.Conditions{Width: 375, DPR: 2, Scheme: schemes[0]}
		b := a
		b.Scheme = schemes[1]
		if err := Comparable(a, b); err == nil || !strings.Contains(err.Error(), "scheme") {
			t.Errorf("schemes %q: got %v, want a scheme refusal", schemes, err)
		}
	}
}

func TestComparableNamesElementTargets(t *testing.T) {
	a := store.Conditions{Width: 375, DPR: 2, Scheme: "dark", Selector: "#cart"}
	if err := Comparable(a, a); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		selector string
		want     string
	}{
		{"", `element "#cart" vs whole viewport`},
		{"#drawer", `element "#cart" vs element "#drawer"`},
	} {
		b := a
		b.Selector = tc.selector
		if err := Comparable(a, b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("selector %q: got %v, want %q", tc.selector, err, tc.want)
		}
	}
}
