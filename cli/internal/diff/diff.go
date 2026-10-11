package diff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"vision/internal/store"
)

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Comparable refuses a diff only when the two shots cannot be laid over each other: a
// different viewport width reflows the page, a different pixel ratio rescales it, a
// different scheme repaints it, and an element shot is not the viewport around it. It
// names each field that differs, because "Not comparable" with no reason reads as a bug
// in the tool rather than a light shot held against a dark baseline.
//
// Height and URL do not refuse. Viewport height drifts with browser chrome, and the same
// screen lives at many URLs (ids, query strings, ports); refusing on those left half of
// all real re-snaps with no diff at all. PNG pads the size difference top-left the way
// Argos and Playwright do, and Changes says what drifted beside the percentage.
//
// An unknown scheme refuses even against another unknown: two failed evals can hide a
// dark shot and a light one, and that is exactly the fake diff this guard exists to stop.
// It costs little, 9 of the first 738 real snaps had no scheme.
func Comparable(a, b store.Conditions) error {
	var reasons []string
	if a.Width != b.Width {
		reasons = append(reasons, fmt.Sprintf("viewport width %d vs %d", a.Width, b.Width))
	}
	if a.DPR != b.DPR {
		reasons = append(reasons, fmt.Sprintf("pixel ratio %g vs %g", a.DPR, b.DPR))
	}
	if a.Scheme != b.Scheme || a.Scheme == "" || b.Scheme == "" {
		reasons = append(reasons, fmt.Sprintf("scheme %q vs %q", a.Scheme, b.Scheme))
	}
	if a.Selector != b.Selector {
		old, cur := "whole viewport", "whole viewport"
		if a.Selector != "" {
			old = fmt.Sprintf("element %q", a.Selector)
		}
		if b.Selector != "" {
			cur = fmt.Sprintf("element %q", b.Selector)
		}
		reasons = append(reasons, old+" vs "+cur)
	}
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf("baseline and this shot differ in %s. Not comparable.", strings.Join(reasons, ", "))
}

// Changes labels what drifted without blocking the diff, so "4% changed" from 140 new
// rows of page is not mistaken for 4% of a fixed viewport. It reads PNG headers only: a
// corrupt image is PNG's error to report, not a label's.
func Changes(base, shot store.Conditions, basePNG, shotPNG []byte) []string {
	var changes []string
	old, oldErr := png.DecodeConfig(bytes.NewReader(basePNG))
	cur, curErr := png.DecodeConfig(bytes.NewReader(shotPNG))
	if oldErr == nil && curErr == nil && (old.Width != cur.Width || old.Height != cur.Height) {
		changes = append(changes, fmt.Sprintf("size %dx%d to %dx%d", old.Width, old.Height, cur.Width, cur.Height))
	}
	if base.URL != shot.URL {
		changes = append(changes, fmt.Sprintf("url %s to %s", base.URL, shot.URL))
	}
	return changes
}

func PNG(a, b []byte) ([]byte, float64, error) {
	old, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		return nil, 0, err
	}
	cur, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	oldBounds, curBounds := old.Bounds(), cur.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, max(oldBounds.Dx(), curBounds.Dx()), max(oldBounds.Dy(), curBounds.Dy())))
	changed := 0
	for y := 0; y < out.Bounds().Max.Y; y++ {
		for x := 0; x < out.Bounds().Max.X; x++ {
			// Missing pixels mark growth or shrinkage even when the other image is transparent.
			p := image.Pt(x, y)
			if p.In(oldBounds) != p.In(curBounds) {
				changed++
				out.SetRGBA(x, y, color.RGBA{R: 251, G: 191, B: 36, A: 255})
				continue
			}
			ca, cb := color.RGBAModel.Convert(old.At(x, y)).(color.RGBA), color.RGBAModel.Convert(cur.At(x, y)).(color.RGBA)
			if ca != cb {
				changed++
				out.SetRGBA(x, y, color.RGBA{R: 244, G: 63, B: 94, A: 255})
			} else {
				g := uint8((uint16(cb.R) + uint16(cb.G) + uint16(cb.B)) / 6)
				out.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
			}
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, out); err != nil {
		return nil, 0, err
	}
	total := out.Bounds().Dx() * out.Bounds().Dy()
	return encoded.Bytes(), float64(changed) / float64(total), nil
}
