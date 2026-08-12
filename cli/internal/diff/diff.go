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

// Comparable refuses a diff when two shots were not taken under the same conditions.
// It names the field that actually differs: the whole struct decides, so reporting only
// the geometry made a scheme or URL mismatch read as "1536x721 @1x, 1536x721 @1x. Not
// comparable.", which looks like a bug in the tool rather than a light shot held against
// a dark baseline.
func Comparable(a, b store.Conditions) error {
	if a == b {
		return nil
	}
	var reasons []string
	if a.Width != b.Width || a.Height != b.Height || a.DPR != b.DPR {
		reasons = append(reasons, fmt.Sprintf("size %dx%d @%gx vs %dx%d @%gx", a.Width, a.Height, a.DPR, b.Width, b.Height, b.DPR))
	}
	if a.URL != b.URL {
		reasons = append(reasons, fmt.Sprintf("url %q vs %q", a.URL, b.URL))
	}
	if a.Scheme != b.Scheme {
		reasons = append(reasons, fmt.Sprintf("scheme %q vs %q", a.Scheme, b.Scheme))
	}
	return fmt.Errorf("baseline and this shot differ in %s. Not comparable.", strings.Join(reasons, ", "))
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
	if old.Bounds() != cur.Bounds() {
		return nil, 0, fmt.Errorf("image dimensions differ: %v and %v", old.Bounds(), cur.Bounds())
	}
	out := image.NewRGBA(cur.Bounds())
	changed := 0
	for y := cur.Bounds().Min.Y; y < cur.Bounds().Max.Y; y++ {
		for x := cur.Bounds().Min.X; x < cur.Bounds().Max.X; x++ {
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
	total := cur.Bounds().Dx() * cur.Bounds().Dy()
	return encoded.Bytes(), float64(changed) / float64(total), nil
}
